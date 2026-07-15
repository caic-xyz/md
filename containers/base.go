// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type base struct {
	name       string
	executable string
	logger     *slog.Logger
	env        []string
}

// Name returns the normalized runtime name.
func (b *base) Name() string {
	return b.name
}

// Executable returns the runtime command used for execution.
func (b *base) Executable() string {
	return b.executable
}

// Run executes a runtime command and returns trimmed stdout.
func (b *base) Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{b.executable}, args...)
	// Command arguments are redacted before logging.
	// codeql[go/clear-text-logging]
	b.logger.Log(ctx, slog.LevelDebug, "exec", "cmd", RedactCommandArgsForLog(cmdArgs))
	cmd := exec.CommandContext(ctx, b.executable, args...) //nolint:gosec // args are from trusted callers.
	cmd.Dir = dir
	cmd.Env = b.commandEnv("LANG=C")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// RunOut executes a runtime command with stdout and stderr connected to writers.
func (b *base) RunOut(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	cmdArgs := append([]string{b.executable}, args...)
	// Command arguments are redacted before logging.
	// codeql[go/clear-text-logging]
	b.logger.Log(ctx, slog.LevelDebug, "exec", "cmd", RedactCommandArgsForLog(cmdArgs))
	cmd := exec.CommandContext(ctx, b.executable, args...) //nolint:gosec // args are from trusted callers.
	cmd.Dir = dir
	cmd.Env = b.commandEnv("LANG=C")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// UntagImage removes an image tag.
func (b *base) UntagImage(ctx context.Context, image string) error {
	_, err := b.Run(ctx, "", "rmi", image)
	return err
}

// IsRootless reports whether the runtime is running rootless.
func (b *base) IsRootless() bool {
	return false
}

// List returns all containers known to the runtime.
func (b *base) List(ctx context.Context) ([]Container, error) {
	out, err := b.Run(ctx, "", "ps", "--all", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var result []Container
	var parseErrs []error
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		ct, err := parseContainerListLine([]byte(line))
		if err != nil {
			parseErrs = append(parseErrs, err)
			continue
		}
		result = append(result, ct)
	}
	if len(result) == 0 && len(parseErrs) > 0 {
		return nil, fmt.Errorf("failed to parse container output: %w", parseErrs[0])
	}
	return result, nil
}

// InspectContainer returns normalized inspect metadata for one container.
func (b *base) InspectContainer(ctx context.Context, name string) (*Container, error) {
	out, err := b.Run(ctx, "", "inspect", name)
	if err != nil {
		return nil, err
	}
	ct, err := ParseInspectContainer([]byte(out))
	if err != nil {
		return nil, err
	}
	if ct.Name == "" {
		ct.Name = name
	}
	return ct, nil
}

// InspectInfo returns detailed normalized inspect metadata for one container.
func (b *base) InspectInfo(ctx context.Context, name string) (*InspectInfo, error) {
	out, err := b.Run(ctx, "", "inspect", name)
	if err != nil {
		return nil, err
	}
	info, err := parseInspectInfo(b.name, name, []byte(out))
	if err != nil {
		return nil, err
	}
	b.fillInspectOSArch(ctx, info)
	return info, nil
}

// HostPort returns the host port mapped to containerPort.
func (b *base) HostPort(ctx context.Context, container, containerPort string) (int32, error) {
	raw, err := b.Run(ctx, "", "inspect", "--format", "{{json .NetworkSettings.Ports}}", container)
	if err != nil {
		return 0, err
	}
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return 0, fmt.Errorf("parsing port map: %w", err)
	}
	bindings := ports[containerPort]
	if len(bindings) == 0 {
		return 0, nil
	}
	port, err := strconv.ParseInt(bindings[0].HostPort, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing host port %q: %w", bindings[0].HostPort, err)
	}
	return int32(port), nil
}

// ImageArchitecture returns the linux architecture for image when available.
func (b *base) ImageArchitecture(ctx context.Context, image string) (string, error) {
	out, err := b.Run(ctx, "", "image", "inspect", image)
	if err != nil {
		return "", err
	}
	return parseImageArchitecture([]byte(out))
}

// RemoteManifestDigest queries the registry for the per-architecture manifest digest without downloading layers.
//
// For a multi-arch image the digest hierarchy is:
//
//	Image Index (manifest list)         sha256:AAA
//	  └── Per-platform Manifest (amd64) sha256:BBB  ← manifest digest
//	        ├── Config                  sha256:CCC  ← docker's {{.Id}}
//	        └── Layers ...
//
// We compare manifest digests (sha256:BBB): this is what "docker pull" prints,
// what {{index .RepoDigests 0}} stores as "repo@sha256:BBB", and what
// "manifest inspect" returns in manifests[].digest. Any change to layers,
// config, or manifest metadata produces a different manifest digest, making it
// a reliable staleness signal.
//
// Both Docker schema v2 manifest lists and OCI image indexes share the same
// "manifests[].{digest, platform}" JSON structure, so one parser covers both
// runtimes and both formats.
func (b *base) RemoteManifestDigest(ctx context.Context, image, arch string) (string, error) {
	out, err := b.Run(ctx, "", "manifest", "inspect", image)
	if err != nil {
		return "", err
	}
	var index manifestIndex
	if err := json.Unmarshal([]byte(out), &index); err != nil {
		return "", fmt.Errorf("parsing manifest inspect output: %w", err)
	}
	for _, m := range index.Manifests {
		if m.Platform.Architecture == arch && m.Platform.OS == "linux" && m.Digest != "" {
			return m.Digest, nil
		}
	}
	if len(index.Manifests) == 1 && index.Manifests[0].Digest != "" {
		return index.Manifests[0].Digest, nil
	}
	return "", fmt.Errorf("no manifest for linux/%s in %s", arch, image)
}

// BaseImageIsLocal reports whether image resolves as a local image tag.
func (b *base) BaseImageIsLocal(ctx context.Context, image string) bool {
	if hasExplicitRegistry(image) {
		return false
	}
	_, err := b.Run(ctx, "", "image", "inspect", "--format", "{{.Id}}", image)
	return err == nil
}

// Stats returns current resource usage for name.
func (b *base) Stats(ctx context.Context, name string) (*Stats, error) {
	out, err := b.Run(ctx, "", "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}", name)
	if err != nil {
		return nil, err
	}
	s, _, err := parseStatsLine(out)
	if err != nil {
		return nil, err
	}
	s.DiskUsed, _ = b.DiskUsage(ctx, name)
	return s, nil
}

// DiskUsage returns the writable container layer size in bytes.
func (b *base) DiskUsage(ctx context.Context, name string) (int64, error) {
	out, err := b.Run(ctx, "", "inspect", "--size", "--format", "{{json .SizeRw}}", name)
	if err != nil {
		return -1, err
	}
	var sz int64
	if err := json.Unmarshal([]byte(out), &sz); err != nil {
		return -1, fmt.Errorf("parsing SizeRw: %w", err)
	}
	return sz, nil
}

// WatchStats streams resource usage for the named running containers.
func (b *base) WatchStats(ctx context.Context, names []string) (iter.Seq2[StatsSample, error], error) {
	args := make([]string, 0, 4+len(names))
	args = append(args, "stats", "--no-trunc", "--format", "{{json .}}")
	args = append(args, names...)
	cmd := b.command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s stats stdout: %w", b.name, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s stats: %w", b.name, err)
	}
	return func(yield func(StatsSample, error) bool) {
		stoppedEarly := false
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			s, name, err := parseStatsLine(line)
			if err != nil {
				_ = yield(StatsSample{}, fmt.Errorf("%s stats: %w", b.name, err))
				stoppedEarly = true
				break
			}
			s.DiskUsed = -1
			if !yield(StatsSample{Name: name, Stats: s}, nil) {
				stoppedEarly = true
				break
			}
		}
		if stoppedEarly && ctx.Err() == nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		scanErr := scanner.Err()
		waitErr := cmd.Wait()
		if stoppedEarly || ctx.Err() != nil {
			return
		}
		if scanErr != nil {
			_ = yield(StatsSample{}, fmt.Errorf("%s stats: %w", b.name, scanErr))
			return
		}
		if waitErr != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				waitErr = fmt.Errorf("%w: %s", waitErr, msg)
			}
			_ = yield(StatsSample{}, fmt.Errorf("%s stats: %w", b.name, waitErr))
		}
	}, nil
}

// StatsAll fetches resource usage for multiple containers in batch.
func (b *base) StatsAll(ctx context.Context, names []string) (map[string]*Stats, error) {
	result := make(map[string]*Stats, len(names))
	if len(names) == 0 {
		return result, nil
	}
	var mu sync.Mutex
	var statsErr, inspectErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		args := make([]string, 0, 5+len(names))
		args = append(args, "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}")
		args = append(args, names...)
		out, err := b.Run(ctx, "", args...)
		if err != nil {
			statsErr = fmt.Errorf("docker stats: %w", err)
			return
		}
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			s, name, err := parseStatsLine(line)
			if err != nil {
				statsErr = fmt.Errorf("docker stats: %w", err)
				return
			}
			mu.Lock()
			if existing, ok := result[name]; ok {
				s.DiskUsed = existing.DiskUsed
			}
			result[name] = s
			mu.Unlock()
		}
	})
	wg.Go(func() {
		args := make([]string, 0, 4+len(names))
		args = append(args, "inspect", "--size", "--format", "{{.Name}}\t{{json .SizeRw}}")
		args = append(args, names...)
		out, err := b.Run(ctx, "", args...)
		if err != nil {
			inspectErr = fmt.Errorf("docker inspect --size: %w", err)
			return
		}
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			name := strings.TrimPrefix(parts[0], "/")
			var sz int64
			if err := json.Unmarshal([]byte(parts[1]), &sz); err != nil {
				continue
			}
			mu.Lock()
			if s, ok := result[name]; ok {
				s.DiskUsed = sz
			} else {
				result[name] = &Stats{DiskUsed: sz}
			}
			mu.Unlock()
		}
	})
	wg.Wait()
	return result, errors.Join(statsErr, inspectErr)
}

// WatchDieEvents streams container die events for containers carrying labelKey.
func (b *base) WatchDieEvents(ctx context.Context, labelKey string) (iter.Seq2[Event, error], error) {
	eventCtx, cancel := context.WithCancel(ctx)
	cmd := b.command(eventCtx,
		"events",
		"--filter", "event=die",
		"--filter", "label="+labelKey,
		"--format", "{{json .}}",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s events stdout: %w", b.name, err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%s events start: %w", b.name, err)
	}
	return func(yield func(Event, error) bool) {
		waited := false
		defer func() {
			cancel()
			if !waited {
				_ = cmd.Wait()
			}
		}()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			ev, ok := parseEvent(scanner.Bytes())
			if !ok {
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil && eventCtx.Err() == nil {
			_ = yield(Event{}, fmt.Errorf("%s events scan: %w", b.name, err))
			return
		}
		waited = true
		if err := cmd.Wait(); err != nil && eventCtx.Err() == nil {
			_ = yield(Event{}, fmt.Errorf("%s events wait: %w", b.name, err))
		}
	}, nil
}

func (b *base) command(ctx context.Context, args ...string) *exec.Cmd {
	cmdArgs := append([]string{b.name}, args...)
	// Command arguments are redacted before logging.
	// codeql[go/clear-text-logging]
	b.logger.Log(ctx, slog.LevelDebug, "exec", "cmd", RedactCommandArgsForLog(cmdArgs))
	cmd := exec.CommandContext(ctx, b.name, args...) //nolint:gosec // args are from trusted callers.
	cmd.Env = b.commandEnv("LANG=C")
	return cmd
}

func (b *base) commandEnv(extra ...string) []string {
	overrides := slices.Clone(b.env)
	overrides = append(overrides, extra...)
	return EnvWithOverrides(os.Environ(), overrides)
}

func (b *base) fillInspectOSArch(ctx context.Context, info *InspectInfo) {
	if info.OS != "" && info.Architecture != "" {
		return
	}
	fallbackOS := info.OS
	if fallbackOS == "" {
		fallbackOS = cleanInspectValue(info.Platform)
	}
	out, err := b.Run(ctx, "", "inspect", info.Name, "--format", "{{.Os}}/{{.Architecture}}")
	if err == nil {
		if observedOS, observedArchitecture, ok := matchingOSArch(out, fallbackOS); ok {
			info.OS = observedOS
			info.Architecture = observedArchitecture
			return
		}
	}
	for _, image := range []string{info.ImageID, info.ImageRef} {
		if image == "" {
			continue
		}
		out, err = b.Run(ctx, "", "image", "inspect", image, "--format", "{{.Os}}/{{.Architecture}}")
		if err != nil {
			continue
		}
		observedOS, observedArchitecture, ok := matchingOSArch(out, fallbackOS)
		if ok {
			info.OS = observedOS
			info.Architecture = observedArchitecture
			return
		}
	}
}

func matchingOSArch(platform, fallbackOS string) (osName, architecture string, ok bool) {
	osName, architecture, ok = splitOSArch(platform, fallbackOS)
	if !ok || fallbackOS != "" && osName != fallbackOS {
		return "", "", false
	}
	return osName, architecture, true
}
