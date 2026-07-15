// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Package containers wraps Docker and Podman command-line runtimes.
package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// EnvWithOverrides applies NAME=value overrides to base.
func EnvWithOverrides(base, overrides []string) []string {
	env := slices.Clone(base)
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

// RedactCommandArgsForLog returns args with sensitive values redacted.
func RedactCommandArgsForLog(args []string) []string {
	redacted := make([]string, len(args))
	redactNext := false
	redactAssignmentNext := false
	for i, arg := range args {
		if redactNext {
			redacted[i] = redactedLogValue
			redactNext = false
			continue
		}
		if redactAssignmentNext {
			redacted[i] = redactAssignmentForLog(arg)
			redactAssignmentNext = false
			continue
		}
		if optionTakesAssignment(arg) {
			redacted[i] = arg
			redactAssignmentNext = true
			continue
		}
		if flag, value, ok := strings.Cut(arg, "="); ok {
			switch {
			case optionTakesAssignment(flag):
				redacted[i] = flag + "=" + redactAssignmentForLog(value)
			case sensitiveOptionName(flag):
				redacted[i] = flag + "=" + redactedLogValue
			default:
				redacted[i] = redactAssignmentForLog(arg)
			}
			continue
		}
		if sensitiveOptionName(arg) {
			redacted[i] = arg
			redactNext = true
			continue
		}
		redacted[i] = redactAssignmentForLog(arg)
	}
	return redacted
}

// New returns a runtime wrapper for executable.
func New(executable string, logger *slog.Logger, env []string) (Runtime, error) {
	switch runtimeName(executable) {
	case "docker":
		return newDocker(executable, logger, env), nil
	case "podman":
		return newPodman(executable, logger, env), nil
	default:
		return &commandRuntime{base: newBase(executable, logger, env)}, nil
	}
}

func newBase(executable string, logger *slog.Logger, env []string) base {
	if logger == nil {
		logger = slog.Default()
	}
	return base{name: runtimeName(executable), executable: executable, logger: logger, env: slices.Clone(env)}
}

func runtimeName(executable string) string {
	if executable == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(executable), ".exe")
}

// parseEvent parses Docker and Podman event JSON.
func parseEvent(data []byte) (Event, bool) {
	var ev eventJSON
	if json.Unmarshal(data, &ev) != nil {
		return Event{}, false
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
		return Event{}, false
	}
	return Event{Name: name, Attributes: attributes}, true
}

// parseImageArchitecture parses runtime image inspect output.
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

// hasExplicitRegistry reports whether image has an explicit registry component.
func hasExplicitRegistry(image string) bool {
	first, _, ok := strings.Cut(image, "/")
	if !ok {
		return false
	}
	return first == "localhost" || strings.ContainsAny(first, ".:")
}

// parseStatsLine parses one JSON line from runtime stats output.
func parseStatsLine(line string) (*Stats, string, error) {
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
	return &Stats{CPUPerc: cpuPerc, MemUsed: memUsed, MemLimit: memLimit, MemPerc: memPerc, PIDs: pids, NetRx: netRx, NetTx: netTx, BlockRead: blockRead, BlockWrite: blockWrite, DiskUsed: -1}, raw.Name, nil
}

// parsePercent parses a percentage string like "1.23%" into 1.23.
func parsePercent(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "N/A" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "%")
	return strconv.ParseFloat(s, 64)
}

// parseMemUsage parses "150MiB / 7.5GiB" into bytes.
func parseMemUsage(s string) (used, limit uint64, err error) {
	if strings.TrimSpace(s) == "N/A / N/A" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected 'used / limit', got %q", s)
	}
	used, err = parseByteSize(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	limit, err = parseByteSize(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return used, limit, nil
}

// parseIOPair parses "1.23kB / 456B" into byte counts.
func parseIOPair(s string) (a, b uint64, err error) {
	if strings.TrimSpace(s) == "N/A / N/A" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected 'a / b', got %q", s)
	}
	a, err = parseByteSize(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	b, err = parseByteSize(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return a, b, nil
}

// parseByteSize parses Docker/Podman byte sizes such as "1.5MiB".
func parseByteSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	for _, u := range byteUnits {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, err
		}
		if f < 0 {
			return 0, fmt.Errorf("negative byte size %q", s)
		}
		return uint64(f * float64(u.mult)), nil
	}
	return 0, fmt.Errorf("unknown byte size suffix in %q", s)
}

// Runtime is a Docker-compatible container runtime.
type Runtime interface {
	// Name returns the normalized runtime name, such as "docker" or "podman".
	Name() string
	// Executable returns the runtime command used for execution.
	Executable() string
	// Run executes a runtime command and returns trimmed stdout.
	Run(ctx context.Context, dir string, args ...string) (string, error)
	// RunOut executes a runtime command with stdout and stderr connected to writers.
	RunOut(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error

	// List returns all containers known to the runtime.
	List(ctx context.Context) ([]Container, error)
	// InspectContainer returns normalized inspect metadata for one container.
	InspectContainer(ctx context.Context, name string) (*Container, error)
	// InspectInfo returns detailed normalized inspect metadata for one container.
	InspectInfo(ctx context.Context, name string) (*InspectInfo, error)
	// HostPort returns the host port mapped to containerPort.
	HostPort(ctx context.Context, containerName, containerPort string) (int32, error)

	// ImageArchitecture returns the linux architecture for image when available.
	ImageArchitecture(ctx context.Context, image string) (string, error)
	// RemoteManifestDigest queries the registry for the per-architecture manifest digest without downloading layers.
	RemoteManifestDigest(ctx context.Context, image, arch string) (string, error)
	// BaseImageIsLocal reports whether image resolves as a local image tag.
	BaseImageIsLocal(ctx context.Context, image string) bool
	// UntagImage removes an image tag.
	UntagImage(ctx context.Context, image string) error

	// Stats returns current resource usage for name.
	Stats(ctx context.Context, name string) (*Stats, error)
	// DiskUsage returns the writable container layer size in bytes.
	DiskUsage(ctx context.Context, name string) (int64, error)
	// StatsAll fetches resource usage for multiple containers in batch.
	StatsAll(ctx context.Context, names []string) (map[string]*Stats, error)
	// WatchStats streams resource usage for the named running containers.
	WatchStats(ctx context.Context, names []string) (iter.Seq2[StatsSample, error], error)
	// WatchDieEvents streams container die events for containers carrying labelKey.
	WatchDieEvents(ctx context.Context, labelKey string) (iter.Seq2[Event, error], error)
	// IsRootless reports whether the runtime is running rootless.
	IsRootless() bool
}

// Event describes a Docker/Podman lifecycle event.
type Event struct {
	Name       string
	Attributes map[string]string
}

// Stats holds runtime resource usage for a container.
type Stats struct {
	CPUPerc    float64 `json:"cpu_perc"`
	MemUsed    uint64  `json:"mem_used"`
	MemLimit   uint64  `json:"mem_limit"`
	MemPerc    float64 `json:"mem_perc"`
	PIDs       int     `json:"pids"`
	NetRx      uint64  `json:"net_rx"`
	NetTx      uint64  `json:"net_tx"`
	BlockRead  uint64  `json:"block_read"`
	BlockWrite uint64  `json:"block_write"`
	DiskUsed   int64   `json:"disk_used"`
}

// StatsSample describes one streamed runtime stats sample.
type StatsSample struct {
	Name  string
	Stats *Stats
}

type commandRuntime struct {
	base
}

type eventJSON struct {
	Name       string            `json:"Name"`
	Attributes map[string]string `json:"Attributes"`
	Actor      struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
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

const redactedLogValue = "<redacted>"

var byteUnits = []struct {
	suffix string
	mult   uint64
}{
	{"KiB", 1 << 10},
	{"MiB", 1 << 20},
	{"GiB", 1 << 30},
	{"TiB", 1 << 40},
	{"kB", 1000},
	{"MB", 1000 * 1000},
	{"GB", 1000 * 1000 * 1000},
	{"TB", 1000 * 1000 * 1000 * 1000},
	{"B", 1},
}

var (
	sensitiveLogAssignmentPattern = regexp.MustCompile(`(?i)^(.*api_key[^=]*=).+$`)
	sensitiveLogNameMarkers       = [...]string{"password", "passwd", "secret", "token", "authkey", "apikey"}
	sensitiveLogNameReplacer      = strings.NewReplacer("_", "", "-", "", ".", "")
)

func optionTakesAssignment(arg string) bool {
	switch arg {
	case "-e", "--env", "--label", "--build-arg":
		return true
	default:
		return false
	}
}

func redactAssignmentForLog(arg string) string {
	name, _, ok := strings.Cut(arg, "=")
	if !ok {
		return arg
	}
	if sensitiveName(name) {
		return name + "=" + redactedLogValue
	}
	if match := sensitiveLogAssignmentPattern.FindStringSubmatch(arg); match != nil {
		return match[1] + redactedLogValue
	}
	return arg
}

func sensitiveOptionName(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	return sensitiveName(strings.TrimLeft(arg, "-"))
}

func sensitiveName(name string) bool {
	compact := sensitiveLogNameReplacer.Replace(strings.ToLower(name))
	for _, marker := range sensitiveLogNameMarkers {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}
