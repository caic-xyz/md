// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Package md manages isolated Docker development containers for AI coding
// agents.
//
// It provides programmatic access to create, manage, and tear down containers
// with SSH access. Containers optionally receive a full git clone of one or
// more repositories; repo-less containers are also supported for general
// agent workloads.
package md

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Logger receives structured log records.
type Logger interface {
	Log(ctx context.Context, level slog.Level, msg string, args ...any)
}

// Client holds global MD tool state (paths, image config, SSH keys).
type Client struct {
	// Paths.
	Home          string
	XDGConfigHome string
	XDGDataHome   string
	XDGStateHome  string

	// SSH key paths.
	HostKeyPath string // ~/.config/md/ssh_host_ed25519_key (generated)
	UserKeyPath string // ~/.ssh/md

	// Container runtime.
	Runtime string // "docker" or "podman"; auto-detected by New().

	// Logger receives md package logs. It must be non-nil.
	Logger Logger

	// ControlMaster enables SSH ControlMaster connection multiplexing.
	// When true, SSH connections are shared via a persistent socket,
	// reducing connection overhead. Disabled by default because stale
	// sockets can cause connectivity issues that are hard to diagnose.
	ControlMaster bool

	// Tokens.
	GithubToken string // GitHub API token for Docker build secrets.
	// TailscaleAPIKey is the Tailscale API key for auth key generation and device deletion.
	//
	// It is necessary to setup ephemeral nodes. The key must be rotated every 90 days.
	//
	// See https://tailscale.com/docs/reference/tailscale-api and
	// https://tailscale.com/docs/features/ephemeral-nodes
	TailscaleAPIKey string

	// DigestCacheTTL controls how long remote image digest lookups are cached.
	// When zero, caching is disabled and the registry is queried on every start.
	DigestCacheTTL time.Duration

	// keysDir is the directory containing SSH host keys and authorized_keys
	// (~/.config/md/), used as a named Docker build context.
	keysDir string

	// env holds extra environment variables appended to subprocess
	// environments (podman, ssh, git, etc.).
	env []string

	// buildMu serializes image build operations (BuildImage, Warmup, and the
	// build step inside Launch) so concurrent callers don't race on the same
	// image tag.
	buildMu sync.Mutex

	// mu protects digestCache and imageBuildCache.
	mu sync.Mutex
	// digestCache caches remote image digest queries to avoid repeated
	// registry network round-trips. Entries expire after DigestCacheTTL.
	digestCache map[string]remoteDigestEntry
	// imageBuildCache stores the last imageBuildNeeded result so that
	// back-to-back checks (e.g. Warmup then Launch) skip redundant
	// docker inspect calls. Protected by mu; invalidated on successful build.
	imageBuildCache *imageBuildCacheEntry
}

// ContainerEvent describes a Docker/Podman lifecycle event for an md container.
type ContainerEvent struct {
	Name       string
	Attributes map[string]string
}

// ContainerStatsSample describes one streamed Docker/Podman stats sample.
type ContainerStatsSample struct {
	Name  string
	Stats *ContainerStats
}

// New creates a Client with global MD tool config and initialises SSH
// infrastructure (keys, authorized_keys, config.d include).
func New(stdout io.Writer) (*Client, error) {
	return newClient("", "", stdout)
}

// newClient is like New but allows overriding the home and runtime. When
// home is empty, os.UserHomeDir() is used and XDG_* env vars are respected.
// When home is explicit, all paths derive from it unconditionally. When rt
// is empty, detectRuntime() is called.
func newClient(home, rt string, stdout io.Writer) (*Client, error) {
	fromEnv := home == ""
	if fromEnv {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	xdgConfigHome := filepath.Join(home, ".config")
	xdgDataHome := filepath.Join(home, ".local", "share")
	xdgStateHome := filepath.Join(home, ".local", "state")
	if fromEnv {
		xdgConfigHome = envOr("XDG_CONFIG_HOME", xdgConfigHome)
		xdgDataHome = envOr("XDG_DATA_HOME", xdgDataHome)
		xdgStateHome = envOr("XDG_STATE_HOME", xdgStateHome)
	}
	if rt == "" {
		rt = detectRuntime(exec.LookPath)
	}
	if rt != "docker" && rt != "podman" {
		return nil, fmt.Errorf("unsupported container runtime %q; supported values: docker, podman", rt)
	}
	c := &Client{
		Home:           home,
		XDGConfigHome:  xdgConfigHome,
		XDGDataHome:    xdgDataHome,
		XDGStateHome:   xdgStateHome,
		HostKeyPath:    filepath.Join(xdgConfigHome, "md", "ssh_host_ed25519_key"),
		UserKeyPath:    filepath.Join(home, ".ssh", "md"),
		Runtime:        rt,
		Logger:         slog.Default(),
		DigestCacheTTL: 12 * time.Hour,
		digestCache:    make(map[string]remoteDigestEntry),
	}
	c.keysDir = filepath.Join(c.XDGConfigHome, "md")
	if err := c.setupSSH(stdout); err != nil {
		return nil, err
	}
	return c, nil
}

// detectRuntime returns the container runtime to use.
// Checks for docker, then podman using the provided lookup function.
func detectRuntime(lookPath func(string) (string, error)) string {
	if _, err := lookPath("docker"); err == nil {
		return "docker"
	}
	if _, err := lookPath("podman"); err == nil {
		return "podman"
	}
	return "docker"
}

// Container returns a Container handle for the given repos.
// It populates MountedPath on each repo from GitRoot if not already set
// (repos is mutated in place). The first repo is the primary. When called
// with no repos, the container has no associated git repository and a
// random name is generated automatically.
// It doesn't start it, it is just a reference.
func (c *Client) Container(repos ...Repo) (*Container, error) {
	// Set MountedPath from GitRoot (base name), disambiguating repos
	// with the same basename using relative paths.
	if err := resolveMountPaths(repos); err != nil {
		return nil, err
	}
	for i := range repos {
		if err := repos[i].Validate(); err != nil {
			return nil, fmt.Errorf("repos[%d]: %w", i, err)
		}
	}
	if len(repos) == 0 {
		var buf [4]byte
		_, _ = rand.Read(buf[:])
		return &Container{
			Client: c,
			Name:   fmt.Sprintf("md-agent-%x", buf),
		}, nil
	}
	return &Container{
		Client: c,
		Repos:  repos,
		Name:   containerName(sanitizeDockerName(filepath.Base(repos[0].MountedPath)), repos[0].Branch),
	}, nil
}

// List returns running md containers sorted by name.
func (c *Client) List(ctx context.Context) ([]*Container, error) {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "ps", "--all", "--no-trunc", "--format", "{{json .}}"})
	if err != nil {
		return nil, err
	}
	var containers []*Container
	var parseErrs []error
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		ct, err := unmarshalContainer(ctx, c, []byte(line))
		if err != nil {
			parseErrs = append(parseErrs, err)
			continue
		}
		if strings.HasPrefix(ct.Name, "md-") {
			ct.Client = c
			ct.sshConfigPath = filepath.Join(c.Home, ".ssh", "config.d", ct.Name+".conf")
			containers = append(containers, &ct)
		}
	}
	if len(containers) == 0 && len(parseErrs) > 0 {
		return nil, fmt.Errorf("failed to parse container output: %w", parseErrs[0])
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, nil
}

// Get returns a single Container by name, or an error if not found.
// Uses docker inspect for a targeted lookup.
func (c *Client) Get(ctx context.Context, name string) (*Container, error) {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", name})
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", name, err)
	}
	ct := &Container{Client: c}
	if err := ct.fillFromInspect(ctx, []byte(out)); err != nil {
		return nil, fmt.Errorf("parsing container %s: %w", name, err)
	}
	if ct.Name == "" {
		ct.Name = name
	}
	ct.sshConfigPath = filepath.Join(c.Home, ".ssh", "config.d", ct.Name+".conf")
	return ct, nil
}

// WatchDieEvents streams container die events for containers carrying labelKey.
func (c *Client) WatchDieEvents(ctx context.Context, labelKey string) (iter.Seq2[ContainerEvent, error], error) {
	eventCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(eventCtx, c.Runtime, "events", //nolint:gosec // Runtime is detected by md; labelKey is passed as a Docker label filter.
		"--filter", "event=die",
		"--filter", "label="+labelKey,
		"--format", "{{json .}}",
	)
	cmd.Env = c.commandEnv("LANG=C")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s events stdout: %w", c.Runtime, err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%s events start: %w", c.Runtime, err)
	}
	return func(yield func(ContainerEvent, error) bool) {
		waited := false
		defer func() {
			cancel()
			if !waited {
				_ = cmd.Wait()
			}
		}()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			ev, ok := parseContainerEvent(scanner.Bytes())
			if !ok {
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil && eventCtx.Err() == nil {
			_ = yield(ContainerEvent{}, fmt.Errorf("%s events scan: %w", c.Runtime, err))
			return
		}
		waited = true
		if err := cmd.Wait(); err != nil && eventCtx.Err() == nil {
			_ = yield(ContainerEvent{}, fmt.Errorf("%s events wait: %w", c.Runtime, err))
		}
	}, nil
}

func parseContainerEvent(data []byte) (ContainerEvent, bool) {
	var ev containerEventJSON
	if json.Unmarshal(data, &ev) != nil {
		return ContainerEvent{}, false
	}
	attributes := ev.Actor.Attributes
	if len(attributes) == 0 {
		attributes = ev.Attributes
	}
	name := attributes["name"]
	if name == "" {
		name = ev.Name
	}
	if name == "" {
		return ContainerEvent{}, false
	}
	return ContainerEvent{Name: name, Attributes: attributes}, true
}

type containerEventJSON struct {
	Name       string            `json:"Name"`
	Attributes map[string]string `json:"Attributes"`
	Actor      struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// BuildImage builds the base Docker images locally for platform:
// first md-root-local, then md-user-local on top of it.
func (c *Client) BuildImage(ctx context.Context, stdout, stderr io.Writer, platform Platform) (retErr error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	platform = platform.Resolve()
	if err := platform.Validate(); err != nil {
		return err
	}
	platformString := platform.String()

	if c.GithubToken == "" {
		_, _ = fmt.Fprintln(stdout, "WARNING: GITHUB_TOKEN not found. Some tools (neovim, rust-analyzer, etc) might fail to install or hit rate limits.")
		_, _ = fmt.Fprintln(stdout, "Please set GITHUB_TOKEN to avoid issues:")
		_, _ = fmt.Fprintln(stdout, "  https://github.com/settings/personal-access-tokens/new?name=md-build-image&description=Token%20to%20help%20generating%20local%20docker%20images%20for%20https://github.com/caic-xyz/md")
		_, _ = fmt.Fprintln(stdout, "  export GITHUB_TOKEN=...")
	}

	// Step 1: build the root image.
	_, _ = fmt.Fprintf(stdout, "- Building root Docker image for %s from rsc/root/Dockerfile ...\n", platformString)
	rootCtx, err := prepareRootBuildContext()
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(rootCtx)) }()
	rootCmd := []string{
		c.Runtime, "build",
		"--platform", platformString,
		"-f", filepath.Join(rootCtx, "Dockerfile"),
		"-t", "md-root-local",
	}
	if c.GithubToken != "" {
		rootCmd = append(rootCmd, "--secret", "id=github_token,env=GITHUB_TOKEN")
	}
	rootCmd = append(rootCmd, rootCtx)
	if err := c.runCmdOut(ctx, "", rootCmd, stdout, stderr); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "- Root image built as 'md-root-local'.")

	// Step 2: build the user image on top of the root image.
	_, _ = fmt.Fprintf(stdout, "- Building user Docker image for %s from rsc/user/Dockerfile ...\n", platformString)
	userCtx, err := prepareBuildContext()
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(userCtx)) }()
	userCmd := []string{
		c.Runtime, "build",
		"--platform", platformString,
		"-f", filepath.Join(userCtx, "Dockerfile"),
		"--build-arg", "BASE_ROOT_IMAGE=md-root-local",
		"-t", "md-user-local",
	}
	if c.GithubToken != "" {
		userCmd = append(userCmd, "--secret", "id=github_token,env=GITHUB_TOKEN")
	}
	userCmd = append(userCmd, userCtx)
	if err := c.runCmdOut(ctx, "", userCmd, stdout, stderr); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "- User image built as 'md-user-local'.")
	c.invalidateImageBuildCache()
	// Clean up BuildKit cache (--mount=type=cache volumes from Dockerfiles).
	// These are only useful during the build itself; pruning avoids leaving
	// orphaned resources on disk.
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "builder", "prune", "-f"}); err != nil {
		_, _ = fmt.Fprintf(stdout, "- Warning: pruning build cache: %v\n", err)
	}
	return nil
}

// WarmupOpts configures base image warmup.
type WarmupOpts struct {
	// BaseImage is the full Docker image reference. When empty,
	// DefaultBaseImage+":latest" is used.
	BaseImage string
	// Platform is the Linux container platform. Empty means use the host's
	// native platform.
	Platform string
	// Caches lists host directories to COPY into the image at build time.
	Caches []CacheMount
	// Quiet suppresses informational output.
	Quiet bool
}

// Warmup ensures the base image is pulled and the user image is built,
// without starting a container. Returns true if a build was performed.
func (c *Client) Warmup(ctx context.Context, stdout, stderr io.Writer, opts *WarmupOpts) (bool, error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	baseImage := opts.BaseImage
	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	p := Platform(opts.Platform).Resolve()
	if err := p.Validate(); err != nil {
		return false, err
	}
	platform := p.String()
	imageName := userImageName(baseImage, activeCacheKey(opts.Caches, c.Home), platform)
	if !c.imageBuildNeeded(ctx, imageName, baseImage, platform, opts.Caches) {
		if !opts.Quiet {
			_, _ = fmt.Fprintf(stdout, "- Docker image %s is up to date, skipping build.\n", imageName)
		}
		return false, nil
	}
	if err := c.buildSpecializedImage(ctx, stdout, stderr, imageName, baseImage, platform, opts.Caches, agentContainerPaths(), opts.Quiet); err != nil {
		return false, err
	}
	c.invalidateImageBuildCache()
	return true, nil
}

// PruneImages removes md-specialized-* and md-fork-* images that are not used by any container.
// Returns the list of removed image names.
func (c *Client) PruneImages(ctx context.Context, stdout, stderr io.Writer) ([]string, error) {
	// List all md-specialized-* and md-fork-* images.
	allImages := make(map[string]struct{})
	for _, prefix := range []string{"md-specialized-*", "md-fork-*"} {
		out, err := c.runCmd(ctx, "", []string{
			c.Runtime, "images", "--format", "{{.Repository}}", "--filter", "reference=" + prefix,
		})
		if err != nil {
			return nil, fmt.Errorf("listing images: %w", err)
		}
		for name := range strings.SplitSeq(out, "\n") {
			if name != "" {
				allImages[name] = struct{}{}
			}
		}
	}
	if len(allImages) == 0 {
		return nil, nil
	}

	// Find images used by running md containers.
	containerOut, err := c.runCmd(ctx, "", []string{
		c.Runtime, "ps", "-a", "--filter", "name=^md-", "--format", "{{.Image}}",
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	inUse := make(map[string]struct{})
	if containerOut != "" {
		for img := range strings.SplitSeq(containerOut, "\n") {
			if img != "" {
				inUse[img] = struct{}{}
			}
		}
	}

	// Remove unused images.
	var removed []string
	for img := range allImages {
		if _, used := inUse[img]; used {
			continue
		}
		if _, err := c.runCmd(ctx, "", []string{c.Runtime, "rmi", img}); err != nil {
			_, _ = fmt.Fprintf(stdout, "- Warning: failed to remove %s: %v\n", img, err)
			continue
		}
		removed = append(removed, img)
	}
	sort.Strings(removed)

	// Clean up BuildKit build cache.
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "builder", "prune", "-f"}); err != nil {
		_, _ = fmt.Fprintf(stdout, "- Warning: pruning build cache: %v\n", err)
	}
	return removed, nil
}

// WatchStats streams resource usage for the named running containers.
//
// DiskUsed is unavailable from the runtime stats stream and is set to -1.
func (c *Client) WatchStats(ctx context.Context, names []string) (iter.Seq2[ContainerStatsSample, error], error) {
	args := make([]string, 0, 5+len(names))
	args = append(args, c.Runtime, "stats", "--no-trunc", "--format", "{{json .}}")
	args = append(args, names...)
	c.Logger.Log(ctx, slog.LevelDebug, "exec", "cmd", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are trusted container names.
	cmd.Env = c.commandEnv("LANG=C")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s stats stdout: %w", c.Runtime, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s stats: %w", c.Runtime, err)
	}
	return func(yield func(ContainerStatsSample, error) bool) {
		stoppedEarly := false
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			s, name, err := parseStatsLine(line)
			if err != nil {
				_ = yield(ContainerStatsSample{}, fmt.Errorf("%s stats: %w", c.Runtime, err))
				stoppedEarly = true
				break
			}
			s.DiskUsed = -1
			if !yield(ContainerStatsSample{Name: name, Stats: s}, nil) {
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
			_ = yield(ContainerStatsSample{}, fmt.Errorf("%s stats: %w", c.Runtime, scanErr))
			return
		}
		if waitErr != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				waitErr = fmt.Errorf("%w: %s", waitErr, msg)
			}
			_ = yield(ContainerStatsSample{}, fmt.Errorf("%s stats: %w", c.Runtime, waitErr))
		}
	}, nil
}

// StatsAll fetches resource usage for multiple containers in batch (2 docker
// calls instead of 2N). Returns a map keyed by container name.
func (c *Client) StatsAll(ctx context.Context, names []string) (map[string]*ContainerStats, error) {
	result := make(map[string]*ContainerStats, len(names))
	if len(names) == 0 {
		return result, nil
	}
	var mu sync.Mutex
	var statsErr, inspectErr error

	var wg sync.WaitGroup

	// Batch docker stats (one call). Stopped containers return zeros.
	wg.Go(func() {
		args := make([]string, 0, 6+len(names))
		args = append(args, c.Runtime, "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}")
		args = append(args, names...)
		out, err := c.runCmd(ctx, "", args)
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
				// Inspect goroutine may have already set DiskUsed; preserve it.
				s.DiskUsed = existing.DiskUsed
			}
			result[name] = s
			mu.Unlock()
		}
	})

	// Batch docker inspect --size (one call).
	wg.Go(func() {
		args := make([]string, 0, 5+len(names))
		args = append(args, c.Runtime, "inspect", "--size", "--format", "{{.Name}}\t{{json .SizeRw}}")
		args = append(args, names...)
		out, err := c.runCmd(ctx, "", args)
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
				result[name] = &ContainerStats{DiskUsed: sz}
			}
			mu.Unlock()
		}
	})

	wg.Wait()
	return result, errors.Join(statsErr, inspectErr)
}

// AgentMounts returns runtime mounts for the provided agent path groups.
//
// It creates the host directories before returning them. The shared agent
// directories are always included; pass values from [HarnessMounts] to include
// harness-specific directories.
func (c *Client) AgentMounts(paths ...AgentPaths) ([]Mount, error) {
	combined := mergePaths(paths)
	mounts := make([]Mount, 0, len(combined.HomePaths)+len(combined.XDGConfigPaths)+len(combined.LocalSharePaths)+len(combined.LocalStatePaths))
	for _, p := range combined.HomePaths {
		hostPath := filepath.Join(c.Home, p)
		if err := os.MkdirAll(hostPath, 0o700); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{HostPath: hostPath, ContainerPath: "/home/user/" + p})
	}
	for _, p := range combined.XDGConfigPaths {
		hostPath := filepath.Join(c.XDGConfigHome, p)
		if err := os.MkdirAll(hostPath, 0o700); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{
			HostPath:      hostPath,
			ContainerPath: "/home/user/.config/" + p,
			ReadOnly:      p == "md",
		})
	}
	for _, p := range combined.LocalSharePaths {
		hostPath := filepath.Join(c.XDGDataHome, p)
		if err := os.MkdirAll(hostPath, 0o700); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{HostPath: hostPath, ContainerPath: "/home/user/.local/share/" + p})
	}
	for _, p := range combined.LocalStatePaths {
		hostPath := filepath.Join(c.XDGStateHome, p)
		if err := os.MkdirAll(hostPath, 0o700); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{HostPath: hostPath, ContainerPath: "/home/user/.local/state/" + p})
	}
	for _, p := range combined.HomePaths {
		if p != ".claude" {
			continue
		}
		claudeJSON := filepath.Join(c.Home, ".claude.json")
		target := filepath.Join(c.Home, ".claude", "claude.json")
		if fi, err := os.Lstat(claudeJSON); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("checking claude.json symlink: %w", err)
			}
			if err := os.Symlink(target, claudeJSON); err != nil {
				return nil, fmt.Errorf("creating claude.json symlink: %w", err)
			}
		} else if fi.Mode()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("file %s exists but is not a symlink", claudeJSON)
		}
		break
	}
	return mounts, nil
}

// getHostPort extracts the host port for containerPort from a running
// container. It uses JSON output instead of Go templates to work around
// Docker 27's "index of untyped nil" bug when port bindings are nil.
func (c *Client) getHostPort(ctx context.Context, container, containerPort string) (int32, error) {
	raw, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", "{{json .NetworkSettings.Ports}}", container})
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

// runCmd executes a command, captures its output, and returns (stdout, error).
// If dir is non-empty, the command runs in that directory.
func (c *Client) runCmd(ctx context.Context, dir string, args []string) (string, error) {
	c.Logger.Log(ctx, slog.LevelDebug, "exec", "cmd", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = c.commandEnv("LANG=C")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// runCmdOut executes a command, directing its stdout and stderr to the given writers.
// If dir is non-empty, the command runs in that directory.
func (c *Client) runCmdOut(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) error {
	c.Logger.Log(ctx, slog.LevelDebug, "exec", "cmd", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = c.commandEnv("LANG=C")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (c *Client) commandEnv(extra ...string) []string {
	overrides := append([]string(nil), c.env...)
	overrides = append(overrides, extra...)
	return envWithOverrides(os.Environ(), overrides)
}

func envWithOverrides(base, overrides []string) []string {
	env := append([]string(nil), base...)
	index := make(map[string]int, len(env))
	for i, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok {
			index[name] = i
		}
	}
	for _, kv := range overrides {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			env = append(env, kv)
			continue
		}
		if i, ok := index[name]; ok {
			env[i] = kv
			continue
		}
		index[name] = len(env)
		env = append(env, kv)
	}
	return env
}

// runGitDir runs a git command with GIT_DIR and GIT_WORK_TREE
// explicitly set, fully decoupling git from the repository config
// (core.worktree). dir is the working directory and also used as
// GIT_WORK_TREE so git never tries to chdir to a non-existent
// submodule worktree.
func (c *Client) runGitDir(ctx context.Context, dir, gitDir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+dir, "LANG=C")
	cmd.Env = append(cmd.Env, c.env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// parseStatsLine parses one JSON line from docker stats output into a
// ContainerStats and returns the container name.
func parseStatsLine(line string) (*ContainerStats, string, error) {
	var raw struct {
		Name     string `json:"Name"`
		CPUPerc  string `json:"CPUPerc"`
		MemUsage string `json:"MemUsage"`
		MemPerc  string `json:"MemPerc"`
		PIDs     string `json:"PIDs"`
		NetIO    string `json:"NetIO"`
		BlockIO  string `json:"BlockIO"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, "", fmt.Errorf("parsing stats JSON: %w", err)
	}
	cpuPerc, err := parsePercent(raw.CPUPerc)
	if err != nil {
		return nil, "", fmt.Errorf("parsing CPU%%: %w", err)
	}
	memPerc, err := parsePercent(raw.MemPerc)
	if err != nil {
		return nil, "", fmt.Errorf("parsing mem%%: %w", err)
	}
	memUsed, memLimit, err := parseMemUsage(raw.MemUsage)
	if err != nil {
		return nil, "", fmt.Errorf("parsing mem usage: %w", err)
	}
	var pids int
	if raw.PIDs != "N/A" {
		pids, err = strconv.Atoi(raw.PIDs)
		if err != nil {
			return nil, "", fmt.Errorf("parsing PIDs: %w", err)
		}
	}
	netRx, netTx, err := parseIOPair(raw.NetIO)
	if err != nil {
		return nil, "", fmt.Errorf("parsing net I/O: %w", err)
	}
	blockRead, blockWrite, err := parseIOPair(raw.BlockIO)
	if err != nil {
		return nil, "", fmt.Errorf("parsing block I/O: %w", err)
	}
	return &ContainerStats{
		CPUPerc:    cpuPerc,
		MemUsed:    memUsed,
		MemLimit:   memLimit,
		MemPerc:    memPerc,
		PIDs:       pids,
		NetRx:      netRx,
		NetTx:      netTx,
		BlockRead:  blockRead,
		BlockWrite: blockWrite,
		DiskUsed:   -1,
	}, raw.Name, nil
}

// cmdErrWithStderr wraps err with the captured stderr from an *exec.ExitError
// so that quiet-mode failures include actionable output.
func cmdErrWithStderr(prefix string, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%s: %w\n%s", prefix, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// extractEmbeddedTree writes an embedded rsc/ subtree to a temp directory.
//
// prefix is the embedded path (e.g. "rsc/user"), tmpPattern is the os.MkdirTemp
// pattern. Returns the temp dir path (caller must clean up).
func extractEmbeddedTree(prefix, tmpPattern string) (dir string, retErr error) {
	tmp, err := os.MkdirTemp("", tmpPattern)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(tmp))
		}
	}()
	err = fs.WalkDir(rscFS, prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, prefix+"/")
		if rel == "" || rel == path {
			return nil
		}
		target := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // matches embedded filesystem permissions
		}
		data, err := rscFS.ReadFile(path)
		if err != nil {
			return err
		}
		// Preserve executable bits for scripts (by extension, shebang, or bin-directory location).
		mode := os.FileMode(0o644)
		if isExecutable(data) {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		return "", fmt.Errorf("extracting %s: %w", prefix, err)
	}
	return tmp, nil
}

// isExecutable reports whether a file from the embedded rsc filesystem should
// be written with execute permission. Matches files starting with a #! shebang.
func isExecutable(data []byte) bool {
	return bytes.HasPrefix(data, []byte("#!"))
}

// prepareBuildContext writes the embedded rsc/user/ tree to a temp directory.
//
// Returns the temp dir path (caller must clean up).
func prepareBuildContext() (string, error) {
	return extractEmbeddedTree("rsc/user", "md-build-*")
}

// prepareRootBuildContext writes the embedded rsc/root/ tree to a temp
// directory.
//
// Returns the temp dir path (caller must clean up).
func prepareRootBuildContext() (string, error) {
	return extractEmbeddedTree("rsc/root", "md-build-root-*")
}

// keysSHA computes a deterministic SHA-256 hash over the SSH key files in
// keysDir. This is used to detect when SSH keys change and trigger an image
// rebuild.
func keysSHA(keysDir string) (string, error) {
	h := sha256.New()
	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub", "authorized_keys"} {
		data, err := os.ReadFile(filepath.Join(keysDir, name)) //nolint:gosec // name is from a hardcoded list
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(h, name)
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) dockerInspectFormat(ctx context.Context, name, format string) (string, error) {
	return c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", name, "--format", format})
}

func (c *Client) getImageVersionLabel(ctx context.Context, imageName string) string {
	out, err := c.dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "org.opencontainers.image.version"}}`)
	if err != nil || out == "" || out == "<no value>" {
		return ""
	}
	return out
}

func (c *Client) imageArchitecture(ctx context.Context, imageName string) (string, error) {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", imageName})
	if err != nil {
		return "", err
	}
	return parseImageArchitecture([]byte(out))
}

func parseImageArchitecture(out []byte) (string, error) {
	var images []imageInspectJSON
	if err := json.Unmarshal(out, &images); err != nil {
		return "", fmt.Errorf("parsing image inspect output: %w", err)
	}
	if len(images) == 0 {
		return "", nil
	}
	if images[0].OS != "" {
		if images[0].OS != "linux" {
			return "", nil
		}
		return images[0].Architecture, nil
	}
	if images[0].ImageManifestDescriptor.Platform.OS != "" && images[0].ImageManifestDescriptor.Platform.OS != "linux" {
		return "", nil
	}
	return images[0].ImageManifestDescriptor.Platform.Architecture, nil
}

// getRemoteManifestDigest queries the registry for the per-architecture
// manifest digest without downloading layers.
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
func (c *Client) getRemoteManifestDigest(ctx context.Context, image, arch string) (string, error) {
	c.Logger.Log(ctx, slog.LevelDebug, "fetching remote manifest digest", "image", image, "arch", arch)
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "manifest", "inspect", image})
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

type remoteDigestEntry struct {
	digest  string
	err     error
	expires time.Time
}

type manifestPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type manifestEntry struct {
	Digest   string           `json:"digest"`
	Platform manifestPlatform `json:"platform"`
}

type manifestIndex struct {
	Manifests []manifestEntry `json:"manifests"`
}

type imageManifestDescriptor struct {
	Platform manifestPlatform `json:"platform"`
}

type imageInspectJSON struct {
	Architecture string `json:"Architecture"`
	OS           string `json:"Os"`

	ImageManifestDescriptor imageManifestDescriptor `json:"ImageManifestDescriptor"`
}

type activeCM struct {
	cm       CacheMount
	hostPath string
	// files lists top-level filenames for Shallow caches. nil for recursive.
	files []string
}

// imageBuildCacheEntry caches the result of imageBuildNeeded so that
// back-to-back calls with the same inputs skip docker inspect exec calls.
type imageBuildCacheEntry struct {
	baseImage  string
	platform   string
	contextSHA string
	cacheKey   string
	needed     bool
}

// cachedRemoteManifestDigest returns the remote per-architecture manifest digest.
// When Client.DigestCacheTTL is non-zero, results are cached for that duration
// to skip repeated registry round-trips. When zero, the registry is always queried.
func (c *Client) cachedRemoteManifestDigest(ctx context.Context, image, arch string) (string, error) {
	if c.DigestCacheTTL == 0 {
		return c.getRemoteManifestDigest(ctx, image, arch)
	}
	key := c.Runtime + "\x00" + image + "\x00" + arch
	c.mu.Lock()
	if e, ok := c.digestCache[key]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.digest, e.err
	}
	c.mu.Unlock()
	digest, err := c.getRemoteManifestDigest(ctx, image, arch)
	c.mu.Lock()
	c.digestCache[key] = remoteDigestEntry{digest: digest, err: err, expires: time.Now().Add(c.DigestCacheTTL)}
	c.mu.Unlock()
	return digest, err
}

// activeCacheKey filters caches to those whose host directories exist and
// returns the cache spec key for the active set.
func activeCacheKey(caches []CacheMount, home string) string {
	_, _, activeKey := resolveCaches(caches, home, nil)
	return activeKey
}

// userImageName returns the Docker image name for a given base image and
// active cache configuration. The name includes a content hash so that
// different base images or cache sets produce distinct images without
// clobbering each other.
func userImageName(baseImage, cacheKey, platform string) string {
	h := sha256.Sum256([]byte(baseImage + "\x00" + cacheKey + "\x00" + platform))
	return "md-specialized-" + hex.EncodeToString(h[:16])
}

// cacheSpecKey returns a short hash over the requested cache specs. Returns
// empty string when caches is nil or empty. Only the spec is hashed, not the
// cache contents.
func cacheSpecKey(caches []CacheMount) string {
	if len(caches) == 0 {
		return ""
	}
	specs := make([]string, len(caches))
	for i, c := range caches {
		specs[i] = cacheSpecString(c)
	}
	sort.Strings(specs)
	h := sha256.Sum256([]byte(strings.Join(specs, ",")))
	return hex.EncodeToString(h[:8])
}

func cacheSpecString(c CacheMount) string {
	return strings.Join([]string{
		c.Name,
		filepath.ToSlash(c.HostPath),
		c.ContainerPath,
		strconv.FormatBool(c.ReadOnly),
		strconv.FormatBool(c.Shallow),
	}, "\x00")
}

type cacheSpecLabelMount struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
	Shallow       bool   `json:"shallow,omitempty"`
}

// activeCacheSpecLabel returns a base64-encoded JSON list of active cache
// specs. It is stored as a label so callers can inspect which caches were
// actually baked into the specialized image, not just compare md.cache_key.
func activeCacheSpecLabel(active []activeCM) string {
	if len(active) == 0 {
		return ""
	}
	mounts := make([]CacheMount, len(active))
	for i, a := range active {
		mounts[i] = a.cm
		mounts[i].HostPath = a.hostPath
	}
	sort.Slice(mounts, func(i, j int) bool {
		return cacheSpecString(mounts[i]) < cacheSpecString(mounts[j])
	})
	labelMounts := make([]cacheSpecLabelMount, len(mounts))
	for i, m := range mounts {
		labelMounts[i] = cacheSpecLabelMount{
			Name:          m.Name,
			Description:   m.Description,
			HostPath:      filepath.ToSlash(m.HostPath),
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
			Shallow:       m.Shallow,
		}
	}
	data, err := json.Marshal(labelMounts)
	if err != nil {
		panic("marshal active cache spec label: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(data)
}

// imageBuildNeeded reports whether the specialized Docker image needs to be
// rebuilt. It checks the base image digest, SSH keys hash, and cache spec
// key against labels on the existing image. For remote base images it also
// verifies the local copy matches the registry.
// home is used to resolve "~/" in cache HostPaths so only caches that
// resolveCaches would inject are compared.
func (c *Client) imageBuildNeeded(ctx context.Context, imageName, baseImage, platform string, caches []CacheMount) bool {
	p := Platform(platform).Resolve()
	if err := p.Validate(); err != nil {
		return true
	}
	platform = p.String()
	// Compute cheap inputs first so we can check the cache.
	contextSHA, err := keysSHA(c.keysDir)
	if err != nil {
		return true
	}
	activeKey := activeCacheKey(caches, c.Home)

	// Check cached result from a previous call with the same inputs.
	c.mu.Lock()
	if e := c.imageBuildCache; e != nil && e.baseImage == baseImage && e.platform == platform && e.contextSHA == contextSHA && e.cacheKey == activeKey {
		needed := e.needed
		c.mu.Unlock()
		return needed
	}
	c.mu.Unlock()

	needed := c.imageBuildNeededSlow(ctx, imageName, baseImage, platform, contextSHA, activeKey)

	c.mu.Lock()
	c.imageBuildCache = &imageBuildCacheEntry{
		baseImage:  baseImage,
		platform:   platform,
		contextSHA: contextSHA,
		cacheKey:   activeKey,
		needed:     needed,
	}
	c.mu.Unlock()
	return needed
}

// invalidateImageBuildCache clears the cached imageBuildNeeded result.
// Must be called after a successful image build so the next check re-evaluates.
func (c *Client) invalidateImageBuildCache() {
	c.mu.Lock()
	c.imageBuildCache = nil
	c.mu.Unlock()
}

// imageBuildNeededSlow performs the full check with docker inspect calls.
func (c *Client) imageBuildNeededSlow(ctx context.Context, imageName, baseImage, platform, contextSHA, activeKey string) bool {
	c.Logger.Log(ctx, slog.LevelDebug, "checking if image build needed", "image", imageName, "base", baseImage)
	// Quick check: does the specialized image have labels at all?
	currentDigest, err := c.dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.base_digest"}}`)
	if err != nil || currentDigest == "" || currentDigest == "<no value>" {
		c.Logger.Log(ctx, slog.LevelDebug, "build needed: no base_digest label", "image", imageName)
		return true
	}
	currentContext, err := c.dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.context_sha"}}`)
	if err != nil || currentContext == "" || currentContext == "<no value>" {
		c.Logger.Log(ctx, slog.LevelDebug, "build needed: no context_sha label", "image", imageName)
		return true
	}

	// Get the base image digest.
	var baseDigest string
	if d, err := c.dockerInspectFormat(ctx, baseImage, "{{index .RepoDigests 0}}"); err == nil && d != "" {
		baseDigest = d
	} else if id, err := c.dockerInspectFormat(ctx, baseImage, "{{.Id}}"); err == nil {
		baseDigest = id
	} else {
		c.Logger.Log(ctx, slog.LevelDebug, "build needed: cannot get base image digest", "base", baseImage)
		return true
	}
	if currentDigest != baseDigest {
		c.Logger.Log(ctx, slog.LevelDebug, "build needed: base digest changed", "current", currentDigest, "base", baseDigest)
		return true
	}

	currentArch, err := c.imageArchitecture(ctx, imageName)
	if err == nil && currentArch != "" {
		expectedArch, err := Platform(platform).Architecture()
		if err != nil {
			return true
		}
		if currentArch != expectedArch {
			c.Logger.Log(ctx, slog.LevelDebug, "build needed: image architecture changed", "current", currentArch, "expected", expectedArch)
			return true
		}
	}

	// For remote images, verify the local base is up to date with the registry.
	// Compare the per-platform manifest digest stored during the last build
	// against the current remote per-platform digest. This avoids the
	// manifest-list-vs-platform-manifest mismatch that occurs when comparing
	// RepoDigests[0] (manifest list digest) against the per-platform entry.
	// Errors are intentionally ignored: a registry failure is not a reason to rebuild;
	// the base digest label comparison above already catches locally-pulled updates.
	isLocal := c.baseImageIsLocal(ctx, baseImage)
	if !isLocal {
		c.Logger.Log(ctx, slog.LevelDebug, "checking remote manifest digest", "base", baseImage)
		storedManifest, err := c.dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.base_manifest_digest"}}`)
		if err == nil && storedManifest != "" && storedManifest != "<no value>" {
			arch, err := Platform(platform).Architecture()
			if err != nil {
				return true
			}
			remoteDigest, err := c.cachedRemoteManifestDigest(ctx, baseImage, arch)
			if err == nil && remoteDigest != storedManifest {
				c.Logger.Log(ctx, slog.LevelDebug, "build needed: remote manifest changed", "stored", storedManifest, "remote", remoteDigest)
				return true
			}
		}
	}

	if currentContext != contextSHA {
		c.Logger.Log(ctx, slog.LevelDebug, "build needed: context SHA changed", "current", currentContext, "expected", contextSHA)
		return true
	}

	currentKey, err := c.dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.cache_key"}}`)
	if err != nil || currentKey == "<no value>" {
		currentKey = ""
	}
	if activeKey != currentKey {
		c.Logger.Log(ctx, slog.LevelDebug, "build needed: cache key changed", "current", currentKey, "expected", activeKey)
		return true
	}

	c.Logger.Log(ctx, slog.LevelDebug, "image is up to date", "image", imageName)
	return false
}

func (c *Client) baseImageIsLocal(ctx context.Context, image string) bool {
	if hasExplicitRegistry(image) {
		return false
	}
	_, err := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{.Id}}", image})
	return err == nil
}

func hasExplicitRegistry(image string) bool {
	first, _, ok := strings.Cut(image, "/")
	if !ok {
		return false
	}
	return first == "localhost" || strings.ContainsAny(first, ".:")
}

// resolveCaches determines which caches have existing host directories and
// computes the set of container directories that need to be pre-created.
// Returns the active caches (with resolved host paths), directories to
// pre-create, and the cache spec key. Caches whose host path does not exist
// are silently skipped.
func resolveCaches(caches []CacheMount, home string, mountPaths []string) (active []activeCM, dirs []string, activeKey string) {
	for _, cm := range caches {
		hostPath := resolveHostPath(cm.HostPath, home)
		if _, err := os.Stat(hostPath); err != nil {
			continue
		}
		cm.ContainerPath = ResolveContainerPath(cm.ContainerPath)
		a := activeCM{cm: cm, hostPath: hostPath}
		if cm.Shallow {
			entries, err := os.ReadDir(hostPath)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					a.files = append(a.files, e.Name())
				}
			}
			if len(a.files) == 0 {
				continue
			}
		}
		active = append(active, a)
	}

	// activeKey reflects only the caches actually injected, not all requested.
	activeMounts := make([]CacheMount, len(active))
	for i, a := range active {
		activeMounts[i] = a.cm
		activeMounts[i].HostPath = a.hostPath
	}
	activeKey = cacheSpecKey(activeMounts)

	// Collect directories to pre-create:
	// - For cache destinations: intermediaries and the leaf itself.
	// - For runtime -v mount targets: the full path (leaf included).
	const base = "/home/user"
	seen := map[string]bool{}
	for _, a := range active {
		seen[a.cm.ContainerPath] = true
		for dir := path.Dir(a.cm.ContainerPath); dir != base && dir != "." && dir != "/"; dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	for _, p := range mountPaths {
		seen[p] = true
	}
	dirs = make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return active, dirs, activeKey
}

// generateDockerfile produces the Dockerfile content for a specialized image.
func generateDockerfile(baseImage string, active []activeCM, dirs []string, baseDigest, contextSHA, activeKey, manifestDigest string) string {
	var df strings.Builder
	fmt.Fprintf(&df, "FROM %s\n", baseImage)
	df.WriteString("COPY --chown=root:root ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key\n")
	df.WriteString("COPY --chown=root:root ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ed25519_key.pub\n")
	df.WriteString("COPY --chown=user:user authorized_keys /home/user/.ssh/authorized_keys\n")
	for _, a := range active {
		owner := "user:user"
		if a.cm.ReadOnly {
			owner = "root:root"
		}
		if a.files != nil {
			// Shallow: copy only top-level files, skip subdirectories.
			// Flags must appear before the JSON array; the array contains only
			// sources and destination.
			for _, f := range a.files {
				fmt.Fprintf(&df, "COPY --from=cache-%s --chown=%s [%q, %q]\n", a.cm.Name, owner, f, a.cm.ContainerPath+"/")
			}
		} else {
			fmt.Fprintf(&df, "COPY --from=cache-%s --chown=%s [\".\", %q]\n", a.cm.Name, owner, a.cm.ContainerPath+"/")
		}
	}
	// Single RUN layer for file permissions and directory pre-creation.
	var run strings.Builder
	run.WriteString("chmod 0600 /etc/ssh/ssh_host_ed25519_key")
	run.WriteString(" && chmod 0644 /etc/ssh/ssh_host_ed25519_key.pub")
	run.WriteString(" && chmod 0400 /home/user/.ssh/authorized_keys")
	if len(dirs) > 0 {
		quoted := make([]string, len(dirs))
		for i, d := range dirs {
			quoted[i] = shellQuote(d)
		}
		joined := strings.Join(quoted, " ")
		fmt.Fprintf(&run, " && mkdir -p %s && chown user:user %s", joined, joined)
	}
	readOnlyPaths := readOnlyCachePaths(active)
	if len(readOnlyPaths) > 0 {
		quoted := make([]string, len(readOnlyPaths))
		for i, p := range readOnlyPaths {
			quoted[i] = shellQuote(p)
		}
		joined := strings.Join(quoted, " ")
		fmt.Fprintf(&run, " && chown -R root:root %s && chmod -R a-w %s", joined, joined)
	}
	fmt.Fprintf(&df, "RUN %s\n", run.String())
	fmt.Fprintf(&df, "LABEL md.base_image=%q\n", baseImage)
	fmt.Fprintf(&df, "LABEL md.base_digest=%q\n", baseDigest)
	fmt.Fprintf(&df, "LABEL md.context_sha=%q\n", contextSHA)
	fmt.Fprintf(&df, "LABEL md.cache_key=%q\n", activeKey)
	fmt.Fprintf(&df, "LABEL md.cache_spec=%q\n", activeCacheSpecLabel(active))
	fmt.Fprintf(&df, "LABEL md.base_manifest_digest=%q\n", manifestDigest)
	df.WriteString("CMD [\"/root/start.sh\"]\n")
	return df.String()
}

func readOnlyCachePaths(active []activeCM) []string {
	seen := make(map[string]struct{})
	for _, a := range active {
		if a.cm.ReadOnly {
			seen[a.cm.ContainerPath] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// buildSpecializedImage builds the per-user Docker image by generating a
// Dockerfile at build time and running "docker build".
//
// Design rationale — three approaches were evaluated:
//
//  1. docker create + docker cp + docker exec + docker commit: avoids BuildKit
//     entirely but "docker cp" is significantly slower than Dockerfile COPY
//     (API round-trips vs storage-driver-level tar streaming). Also requires
//     starting the container to fix file ownership, adding latency.
//
//  2. Maintained Dockerfile in rsc/specialized/ with BuildKit: fast COPY but
//     requires keeping the file in sync with runtime logic (which caches
//     exist, what mount paths to create). BuildKit's persistent build cache
//     also accumulates multi-GB of intermediate layers over repeated rebuilds,
//     requiring periodic "docker builder prune" and slowing subsequent builds
//     as the cache grows.
//
//  3. Maintained Dockerfile in rsc/specialized/ without BuildKit: avoids
//     BuildKit cache growth but cannot adapt to dynamic inputs and still
//     requires keeping the file in sync with runtime logic.
//
//  4. Generated Dockerfile + docker build (current): combines COPY's speed with
//     dynamic generation. Uses --build-context per cache directory so large host
//     trees are read in-place without copying into the build context. COPY
//     --chown sets ownership at copy time, eliminating the container
//     start/exec/stop cycle. --no-cache prevents stale layer reuse and keeps
//     BuildKit's residual cache small.
//
// keysDir contains SSH host keys and authorized_keys. home resolves "~/" in
// cache HostPaths. mountPaths lists container-side -v mount targets to
// pre-create with user ownership.
func (c *Client) buildSpecializedImage(ctx context.Context, stdout, stderr io.Writer, imageName, baseImage, platform string, caches []CacheMount, mountPaths []string, quiet bool) error {
	c.Logger.Log(ctx, slog.LevelDebug, "building specialized image", "image", imageName, "base", baseImage)
	p := Platform(platform).Resolve()
	if err := p.Validate(); err != nil {
		return err
	}
	if err := validateCacheMounts(caches); err != nil {
		return err
	}
	platform = p.String()
	arch, err := Platform(platform).Architecture()
	if err != nil {
		return err
	}
	// References without an explicit registry are ambiguous: they may name a
	// local image tag or a Docker Hub repository. Prefer an existing local
	// image; otherwise pull from the default registry.
	isLocal := c.baseImageIsLocal(ctx, baseImage)
	remoteBasePulled := false
	if isLocal {
		if _, err := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{.Id}}", baseImage}); err != nil {
			return fmt.Errorf("local image %s not found; build it first with 'md build-image'", baseImage)
		}
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "- Using local base image %s.\n", baseImage)
		}
	} else {
		// Compare the local image ID before and after pull to detect changes.
		idBefore, _ := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{.Id}}", baseImage})
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "- Pulling base image %s ...\n", baseImage)
		}
		var pullErr error
		if quiet {
			if _, err := c.runCmd(ctx, "", []string{c.Runtime, "pull", "--platform", platform, baseImage}); err != nil {
				pullErr = cmdErrWithStderr("pulling base image", err)
			}
		} else {
			if err := c.runCmdOut(ctx, "", []string{c.Runtime, "pull", "--platform", platform, baseImage}, stdout, stderr); err != nil {
				pullErr = fmt.Errorf("pulling base image: %w", err)
			}
		}
		if pullErr != nil {
			if idBefore == "" {
				return pullErr
			}
			c.Logger.Log(ctx, slog.LevelWarn, "failed to pull base image; using local copy", "image", baseImage, "err", pullErr)
			if !quiet {
				_, _ = fmt.Fprintf(stdout, "- Warning: failed to pull base image %s; using local copy.\n", baseImage)
			}
		} else {
			remoteBasePulled = true
			idAfter, _ := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{.Id}}", baseImage})
			if !quiet {
				if idBefore != "" && idBefore == idAfter {
					_, _ = fmt.Fprintf(stdout, "  Base image is up to date.\n")
				} else if v := c.getImageVersionLabel(ctx, baseImage); strings.HasPrefix(v, "v") {
					_, _ = fmt.Fprintf(stdout, "  Version: %s\n", v)
				}
			}
		}
	}

	c.Logger.Log(ctx, slog.LevelDebug, "base image ready, fetching base image digest")
	// Get base image digest for label.
	baseDigest, err := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{index .RepoDigests 0}}", baseImage})
	if err != nil || baseDigest == "" {
		baseDigest, _ = c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{.Id}}", baseImage})
	}
	var manifestDigest string
	if remoteBasePulled {
		manifestDigest, _ = c.getRemoteManifestDigest(ctx, baseImage, arch)
	}

	contextSHA, err := keysSHA(c.keysDir)
	if err != nil {
		return fmt.Errorf("computing keys SHA: %w", err)
	}

	active, dirs, activeKey := resolveCaches(caches, c.Home, mountPaths)

	if !quiet {
		_, _ = fmt.Fprintf(stdout, "- Building container image %s from %s ...\n", imageName, baseImage)
		// Report skipped caches (host dir does not exist).
		activeNames := make(map[string]bool, len(active))
		for _, a := range active {
			activeNames[a.cm.Name] = true
		}
		for _, cm := range caches {
			if !activeNames[cm.Name] {
				_, _ = fmt.Fprintf(stdout, "  Cache %s: %s not found, skipping\n", cm.Name, resolveHostPath(cm.HostPath, c.Home))
			}
		}
		for _, a := range active {
			var files int64
			var size int64
			if a.files != nil {
				// Shallow: only top-level files are copied.
				files = int64(len(a.files))
				for _, f := range a.files {
					if info, err := os.Stat(filepath.Join(a.hostPath, f)); err == nil {
						size += info.Size()
					}
				}
			} else {
				files, size = dirStats(a.hostPath)
			}
			_, _ = fmt.Fprintf(stdout, "  Cache %s: %s files, %s\n", a.cm.Name, formatCount(files), FormatBytes(size))
		}
	}

	// Generate a temporary build context containing SSH keys and a Dockerfile.
	// Cache directories are mounted via --build-context so their contents are
	// read directly from the host without copying into the context dir.
	tmpDir, err := os.MkdirTemp("", "md-specialized-*")
	if err != nil {
		return fmt.Errorf("creating build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub", "authorized_keys"} {
		data, err := os.ReadFile(filepath.Join(c.keysDir, name)) //nolint:gosec // name is from a hardcoded list
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, filepath.Base(name)), data, 0o600); err != nil { //nolint:gosec // name is from a hardcoded list
			return fmt.Errorf("staging %s: %w", name, err)
		}
	}

	df := generateDockerfile(baseImage, active, dirs, baseDigest, contextSHA, activeKey, manifestDigest)
	c.Logger.Log(ctx, slog.LevelDebug, "generated Dockerfile", "content", df)

	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(df), 0o644); err != nil { //nolint:gosec // Dockerfile is ephemeral, world-readable is fine
		return fmt.Errorf("writing Dockerfile: %w", err)
	}

	// Build the image. --no-cache forces all layers to rebuild (prevents stale
	// results). We omit --pull so BuildKit won't re-pull the base (we already
	// pulled above).
	buildCmd := []string{c.Runtime, "build", "--no-cache", "--platform", platform, "-t", imageName}
	for _, a := range active {
		buildCmd = append(buildCmd, "--build-context", fmt.Sprintf("cache-%s=%s", a.cm.Name, filepath.ToSlash(a.hostPath)))
	}
	buildCmd = append(buildCmd, filepath.ToSlash(tmpDir))

	if quiet {
		if _, err := c.runCmd(ctx, "", buildCmd); err != nil {
			buildErr := cmdErrWithStderr("building image", err)
			if isStaleBuilderCacheErr(buildErr) {
				if _, pruneErr := c.runCmd(ctx, "", []string{c.Runtime, "builder", "prune", "-f"}); pruneErr != nil {
					return buildErr
				}
				if _, err2 := c.runCmd(ctx, "", buildCmd); err2 != nil {
					return cmdErrWithStderr("building image", err2)
				}
				return nil
			}
			return buildErr
		}
	} else {
		if err := c.runCmdOut(ctx, "", buildCmd, stdout, stderr); err != nil {
			buildErr := fmt.Errorf("building image: %w", err)
			if isStaleBuilderCacheErr(buildErr) {
				_, _ = fmt.Fprintln(stdout, "- Stale BuildKit cache detected; pruning and retrying ...")
				if _, pruneErr := c.runCmd(ctx, "", []string{c.Runtime, "builder", "prune", "-f"}); pruneErr != nil {
					return buildErr
				}
				if err2 := c.runCmdOut(ctx, "", buildCmd, stdout, stderr); err2 != nil {
					return fmt.Errorf("building image: %w", err2)
				}
				return nil
			}
			return buildErr
		}
	}
	return nil
}

// setupSSH ensures SSH keys, authorized_keys, and ~/.ssh/config.d exist.
// Called once by New(); idempotent.
func (c *Client) setupSSH(stdout io.Writer) error {
	for _, d := range []string{
		filepath.Dir(c.HostKeyPath), // ~/.config/md/
		filepath.Join(c.Home, ".ssh", "config.d"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	sshDir := filepath.Join(c.Home, ".ssh")
	if err := ensureSSHConfigInclude(stdout, sshDir); err != nil {
		return err
	}
	if err := ensureEd25519Key(stdout, c.UserKeyPath, "md-user"); err != nil {
		return err
	}
	if err := ensureEd25519Key(stdout, c.HostKeyPath, "md-host"); err != nil {
		return err
	}
	pubKey, err := os.ReadFile(c.UserKeyPath + ".pub")
	if err != nil {
		return err
	}
	authKeysPath := filepath.Join(c.keysDir, "authorized_keys")
	if existing, _ := os.ReadFile(authKeysPath); bytes.Equal(existing, pubKey) { //nolint:gosec // path is from trusted config dir
		return nil
	}
	return os.WriteFile(authKeysPath, pubKey, 0o600) //nolint:gosec // path is constructed from trusted config dir
}

// isStaleBuilderCacheErr reports whether err looks like a BuildKit cache
// corruption error caused by a file that existed in a previous build context
// snapshot but has since been deleted from the host. This most commonly affects
// shallow caches: because each file gets its own COPY instruction, BuildKit
// stores per-file refs; if any of those files is later deleted, the next build
// fails to checksum the stale ref. Non-shallow caches copy "." so deleted files
// fall out naturally without leaving dangling refs.
func isStaleBuilderCacheErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "failed to compute cache key") || strings.Contains(s, "failed to calculate checksum of ref")
}

// dirStats returns the number of regular files and total byte size under dir.
// Unreadable entries are silently skipped.
func dirStats(dir string) (files, n int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				files++
				n += info.Size()
			}
		}
		return nil
	})
	return files, n
}

// formatCount formats n with comma thousands separators (e.g. 1234567 → "1,234,567").
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := len(s) % 3
	var b strings.Builder
	b.Grow(len(s) + len(s)/3)
	if start > 0 {
		b.WriteString(s[:start])
	}
	for i := start; i < len(s); i += 3 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// ResolveContainerPath expands "~" or a leading "~/" to the container user's
// home directory; absolute paths are returned unchanged.
func ResolveContainerPath(p string) string {
	suffix, ok := homePathSuffix(p, false)
	if !ok {
		return p
	}
	if suffix == "" {
		return "/home/user"
	}
	return path.Join("/home/user", suffix)
}

// resolveHostPath expands "~" or a leading "~/" (or "~\" on Windows) to home;
// absolute paths are returned unchanged.
func resolveHostPath(p, home string) string {
	suffix, ok := homePathSuffix(p, true)
	if !ok {
		return p
	}
	return filepath.ToSlash(filepath.Join(home, suffix))
}

func homePathSuffix(p string, windowsBackslash bool) (string, bool) {
	if p == "~" {
		return "", true
	}
	if strings.HasPrefix(p, "~/") {
		return p[2:], true
	}
	if windowsBackslash && strings.HasPrefix(p, `~\`) {
		return p[2:], true
	}
	return "", false
}

// Harness identifies an agent harness whose config directories are mounted
// into a container.
type Harness string

// Known agent harnesses.
const (
	HarnessAmp      Harness = "amp"
	HarnessAndroid  Harness = "android"
	HarnessClaude   Harness = "claude"
	HarnessCodex    Harness = "codex"
	HarnessGemini   Harness = "gemini"
	HarnessGoose    Harness = "goose"
	HarnessKilo     Harness = "kilo"
	HarnessKimi     Harness = "kimi"
	HarnessOpencode Harness = "opencode"
	HarnessPi       Harness = "pi"
	HarnessQwen     Harness = "qwen"
)

// AgentPaths groups the relative host directory paths for one or more agent
// harnesses. Paths under HomePaths are relative to $HOME, XDGConfigPaths to
// $XDG_CONFIG_HOME (~/.config), LocalSharePaths to $XDG_DATA_HOME
// (~/.local/share), and LocalStatePaths to $XDG_STATE_HOME (~/.local/state).
type AgentPaths struct {
	// Description is a short human-readable label for the harness (e.g.
	// "Claude Code"). Displayed in settings UI.
	Description     string
	HomePaths       []string
	XDGConfigPaths  []string
	LocalSharePaths []string
	LocalStatePaths []string
}

// HarnessMounts maps each known harness to its path configuration.
var HarnessMounts = map[Harness]AgentPaths{
	HarnessAmp:      {Description: "Amp", HomePaths: []string{".amp"}, XDGConfigPaths: []string{"amp"}, LocalSharePaths: []string{"amp"}},
	HarnessAndroid:  {Description: "Android Studio", HomePaths: []string{".android"}},
	HarnessClaude:   {Description: "Claude Code", HomePaths: []string{".claude"}},
	HarnessCodex:    {Description: "Codex", HomePaths: []string{".codex"}},
	HarnessGemini:   {Description: "Gemini CLI", HomePaths: []string{".gemini"}},
	HarnessGoose:    {Description: "Goose", XDGConfigPaths: []string{"goose"}, LocalSharePaths: []string{"goose"}},
	HarnessKilo:     {Description: "Kilo Code", HomePaths: []string{".kilocode"}},
	HarnessKimi:     {Description: "Kimi", HomePaths: []string{".kimi"}},
	HarnessOpencode: {Description: "OpenCode", XDGConfigPaths: []string{"opencode"}, LocalSharePaths: []string{"opencode"}, LocalStatePaths: []string{"opencode"}},
	HarnessPi:       {Description: "Pi", HomePaths: []string{".pi"}},
	HarnessQwen:     {Description: "Qwen Code", HomePaths: []string{".qwen"}},
}

// Mount defines a host directory to bind-mount into the running container.
type Mount struct {
	// HostPath is the absolute path on the host. "~" and "~/" resolve to the
	// host user's home directory.
	HostPath string
	// ContainerPath is the absolute path inside the container. "~" and "~/"
	// resolve to the container user's home directory via [ResolveContainerPath].
	ContainerPath string
	// ReadOnly mounts the host path read-only.
	ReadOnly bool
}

func (m *Mount) dockerArg(home string) (string, error) {
	hostPath := resolveHostPath(m.HostPath, home)
	if hostPath == "" {
		return "", errors.New("mount host path is empty")
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		return "", fmt.Errorf("mount host path %q: %w", hostPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("mount host path %q is not a directory", hostPath)
	}
	containerPath := ResolveContainerPath(m.ContainerPath)
	if containerPath == "" {
		return "", errors.New("mount container path is empty")
	}
	if !path.IsAbs(containerPath) {
		return "", fmt.Errorf("mount container path %q is not absolute", containerPath)
	}
	arg := filepath.ToSlash(hostPath) + ":" + containerPath
	if m.ReadOnly {
		arg += ":ro"
	}
	return arg, nil
}

// CacheMount defines a host directory to copy into the specialized container
// image. Well-known caches are defined in [WellKnownCaches]; custom caches can
// be constructed directly.
type CacheMount struct {
	// Name identifies the cache in progress output and Docker build contexts.
	// It must match [a-z0-9][a-z0-9-]*, e.g. "go-mod".
	Name string
	// Description is a short human-readable label for the cache group (e.g.
	// "Go module cache"). Displayed in settings UI.
	Description string
	// HostPath is the absolute path on the host. "~" and "~/" resolve to the
	// host user's home directory.
	HostPath string
	// ContainerPath is the absolute path inside the container. "~" and "~/"
	// resolve to the container user's home directory via [ResolveContainerPath].
	ContainerPath string
	// ReadOnly copies the cache into the image as root-owned, non-writable files.
	ReadOnly bool
	// Shallow copies only top-level files from HostPath, ignoring
	// subdirectories. Useful for directories like ~/.android where only a few
	// files (debug.keystore, adbkey) are needed but subdirectories (avd/,
	// cache/) are large and unwanted.
	Shallow bool
}

// validateCacheMounts rejects names that cannot be used as Docker build
// context and stage identifiers. Without this, invalid user-provided names fail
// later as opaque Docker "invalid reference format" errors.
func validateCacheMounts(caches []CacheMount) error {
	for _, c := range caches {
		if !reCacheMountName.MatchString(c.Name) {
			return fmt.Errorf("cache mount name %q is invalid; use [a-z0-9][a-z0-9-]*", c.Name)
		}
	}
	return nil
}

// WellKnownCaches is the set of predefined build-tool caches, keyed by short
// name. Each name may expand to multiple [CacheMount]s (e.g. "cargo" covers
// both the registry index and git sources). HostPath values use "~/" as a
// prefix that [Container.Launch] resolves to the host user's home directory at
// runtime; ContainerPath values may use "~/" for the container user's home
// directory.
var WellKnownCaches = map[string][]CacheMount{
	"android-keys": {
		{Name: "android-keys", Description: "Android debug keystore and ADB keys", HostPath: "~/.android", ContainerPath: "/home/user/.android", Shallow: true},
	},
	"bun": {
		{Name: "bun", Description: "Bun package manager", HostPath: "~/.bun/install/cache", ContainerPath: "/home/user/.bun/install/cache"},
	},
	"cargo": {
		{Name: "cargo-registry", Description: "Rust cargo registry and git", HostPath: "~/.cargo/registry", ContainerPath: "/home/user/.cargo/registry"},
		{Name: "cargo-git", Description: "Rust cargo registry and git", HostPath: "~/.cargo/git", ContainerPath: "/home/user/.cargo/git"},
	},
	// "go-build": {
	// 	{Name: "go-build", Description: "Go build cache", HostPath: "~/.cache/go-build", ContainerPath: "/home/user/.cache/go-build"},
	// },
	"go-mod": {
		{Name: "go-mod", Description: "Go module cache", HostPath: "~/go/pkg/mod", ContainerPath: "/home/user/go/pkg/mod"},
	},
	"gradle": {
		{Name: "gradle-caches", Description: "Gradle caches and wrapper", HostPath: "~/.gradle/caches", ContainerPath: "/home/user/.gradle/caches"},
		{Name: "gradle-wrapper", Description: "Gradle caches and wrapper", HostPath: "~/.gradle/wrapper/dists", ContainerPath: "/home/user/.gradle/wrapper/dists"},
	},
	"maven": {
		{Name: "maven", Description: "Maven repository", HostPath: "~/.m2/repository", ContainerPath: "/home/user/.m2/repository"},
	},
	"npm": {
		{Name: "npm", Description: "npm cache", HostPath: "~/.npm", ContainerPath: "/home/user/.npm"},
	},
	"pip": {
		{Name: "pip", Description: "Python pip cache", HostPath: "~/.cache/pip", ContainerPath: "/home/user/.cache/pip"},
	},
	"pnpm": {
		{Name: "pnpm", Description: "pnpm store", HostPath: "~/.local/share/pnpm/store", ContainerPath: "/home/user/.local/share/pnpm/store"},
	},
	"uv": {
		{Name: "uv", Description: "UV Python package manager", HostPath: "~/.cache/uv", ContainerPath: "/home/user/.cache/uv"},
	},
}

//

var (
	reCacheMountName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	reInvalid        = regexp.MustCompile(`[/@#:~]+`)
	reStripRemaining = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
	reCollapse       = regexp.MustCompile(`[-_.]{2,}`)
	reGitAt          = regexp.MustCompile(`^git@([^:]+):(.+)$`)
	reSSHGit         = regexp.MustCompile(`^ssh://git@([^/]+)/(.+)$`)
	reGitProto       = regexp.MustCompile(`^git://([^/]+)/(.+)$`)
)

// alwaysPaths are merged into every container's mount set automatically.
// Callers do not need to include these; Client methods add them internally.
var alwaysPaths = AgentPaths{
	XDGConfigPaths: []string{"agents", "md"},
}

// mergePaths concatenates a slice of AgentPaths into one, prepending alwaysPaths.
func mergePaths(paths []AgentPaths) AgentPaths {
	result := alwaysPaths
	for _, p := range paths {
		result.HomePaths = append(result.HomePaths, p.HomePaths...)
		result.XDGConfigPaths = append(result.XDGConfigPaths, p.XDGConfigPaths...)
		result.LocalSharePaths = append(result.LocalSharePaths, p.LocalSharePaths...)
		result.LocalStatePaths = append(result.LocalStatePaths, p.LocalStatePaths...)
	}
	return result
}

// agentContainerPaths returns the container-side mount target paths for all
// agent config mounts. These are the -v targets that must be pre-created with
// user ownership in the Docker image before docker run creates them as root.
func agentContainerPaths() []string {
	all := alwaysPaths
	for _, p := range HarnessMounts {
		all.HomePaths = append(all.HomePaths, p.HomePaths...)
		all.XDGConfigPaths = append(all.XDGConfigPaths, p.XDGConfigPaths...)
		all.LocalSharePaths = append(all.LocalSharePaths, p.LocalSharePaths...)
		all.LocalStatePaths = append(all.LocalStatePaths, p.LocalStatePaths...)
	}
	paths := make([]string, 0, len(all.HomePaths)+len(all.XDGConfigPaths)+len(all.LocalSharePaths)+len(all.LocalStatePaths))
	for _, p := range all.HomePaths {
		paths = append(paths, "/home/user/"+p)
	}
	for _, p := range all.XDGConfigPaths {
		paths = append(paths, "/home/user/.config/"+p)
	}
	for _, p := range all.LocalSharePaths {
		paths = append(paths, "/home/user/.local/share/"+p)
	}
	for _, p := range all.LocalStatePaths {
		paths = append(paths, "/home/user/.local/state/"+p)
	}
	return paths
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// sanitizeDockerName sanitizes a string for use in a Docker container name.
//
// Docker container names must match [a-zA-Z0-9][a-zA-Z0-9_.-].
func sanitizeDockerName(name string) string {
	s := reInvalid.ReplaceAllString(name, "-")
	s = reStripRemaining.ReplaceAllString(s, "")
	s = reCollapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	if s == "" {
		return "unnamed"
	}
	return s
}

// containerName returns the container name for a repo and branch.
func containerName(repoName, branchName string) string {
	return "md-" + sanitizeDockerName(repoName) + "-" + sanitizeDockerName(branchName)
}
