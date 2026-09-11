// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for the md CLI tool.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/containers"
	"github.com/caic-xyz/md/git"
)

var testLogStart = time.Now()

const (
	fakePruneRuntimeEnv    = "MD_TEST_FAKE_PRUNE_RUNTIME"
	fakePruneRuntimeLogEnv = "MD_TEST_FAKE_PRUNE_RUNTIME_LOG"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakePruneRuntimeEnv) == "1" {
		os.Exit(runFakePruneRuntime(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestCmdPrune(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker", "podman"} {
		path := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			path += ".exe"
		}
		if err := copyTestExecutable(exe, path); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(dir, "commands.log")
	t.Setenv(fakePruneRuntimeEnv, "1")
	t.Setenv(fakePruneRuntimeLogEnv, logPath)
	t.Setenv("PATH", dir)
	if err := (&app{}).cmdPrune(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath) //nolint:gosec // logPath is generated in a private test directory.
	if err != nil {
		t.Fatal(err)
	}
	for _, runtimeName := range []string{"docker", "podman"} {
		for _, command := range []string{"images ", "ps -a ", "rmi md-specialized-unused", "builder prune -f"} {
			if want := runtimeName + " " + command; !strings.Contains(string(log), want) {
				t.Errorf("commands = %q, want %q", log, want)
			}
		}
	}
}

func copyTestExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // test binary is copied to a private temporary directory.
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // test executable is written to a private temporary directory.
	if err != nil {
		return errors.Join(err, in.Close())
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close(), in.Close())
}

func runFakePruneRuntime(args []string) int {
	logPath := os.Getenv(fakePruneRuntimeLogEnv)
	if logPath == "" {
		_, _ = fmt.Fprintln(os.Stderr, "missing fake prune runtime log path")
		return 1
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test path comes from a private temporary directory.
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "opening fake prune runtime log: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(log, strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"), strings.Join(args, " ")); err != nil {
		_ = log.Close()
		_, _ = fmt.Fprintf(os.Stderr, "writing fake prune runtime log: %v\n", err)
		return 1
	}
	if err := log.Close(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "closing fake prune runtime log: %v\n", err)
		return 1
	}
	switch args[0] {
	case "images":
		_, _ = fmt.Fprintln(os.Stdout, "sha256:unused\tmd-specialized-unused")
	case "ps", "rmi", "builder":
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unexpected fake prune runtime command: %s\n", strings.Join(args, " "))
		return 1
	}
	return 0
}

func testLogger(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(testLogWriter{t: t}, testLoggerOptions()))
}

func testLoggerOptions() *slog.HandlerOptions {
	return &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Int64("ms", time.Since(testLogStart).Milliseconds())
			}
			return a
		},
	}
}

func mustRuntime(t testing.TB, logger *slog.Logger, name string) containers.Runtime {
	r, err := containers.New(name, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type testLogWriter struct {
	t testing.TB
}

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func TestContainerFlags(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		platform := "linux/amd64"
		cf := &containerFlags{platform: &platform}
		got, err := cf.containerPlatform()
		if err != nil {
			t.Fatal(err)
		}
		if got != "linux/amd64" {
			t.Errorf("containerPlatform = %q, want linux/amd64", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		platform := "x64"
		cf := &containerFlags{platform: &platform}
		if _, err := cf.containerPlatform(); err == nil {
			t.Fatal("expected error for x64 alias")
		}
	})
}

func TestNewRunContainer(t *testing.T) {
	t.Parallel()
	logger := testLogger(t)
	client := &md.Client{Logger: logger, Runtime: mustRuntime(t, logger, "docker")}
	source := &md.Container{
		Client: client,
		Logger: logger.With(slog.String("cntr", "source")),
		Repos: []md.Repo{
			{GitRoot: "/src/one", Branches: []string{"main"}, ContainerPath: "/home/user/src/one"},
			{GitRoot: "/src/two", Branches: []string{"feature"}, ContainerPath: "/home/user/src/two"},
		},
	}
	got, err := newRunContainer(source)
	if err != nil {
		t.Fatal(err)
	}
	if got.Client != client {
		t.Fatal("Client was not preserved")
	}
	if got.Logger == nil {
		t.Fatal("Logger was not initialized")
	}
	if !strings.HasPrefix(got.Name, "md-one-run-") {
		t.Fatalf("Name = %q, want md-one-run-*", got.Name)
	}
	if !reflect.DeepEqual(got.Repos, source.Repos) {
		t.Fatalf("Repos = %+v, want %+v", got.Repos, source.Repos)
	}
	got.Repos[0].GitRoot = "changed"
	if source.Repos[0].GitRoot == "changed" {
		t.Fatal("Repos aliases source slice")
	}
}

func TestTristateBool(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		var b tristateBool
		if b.String() != "unset" {
			t.Fatalf("zero String = %q, want unset", b.String())
		}
		if err := b.Set("false"); err != nil {
			t.Fatal(err)
		}
		if !b.set || b.value {
			t.Fatalf("Set(false): set=%v value=%v, want set true value false", b.set, b.value)
		}
		if got := b.String(); got != "false" {
			t.Fatalf("String = %q, want false", got)
		}
		if err := b.Set("true"); err != nil {
			t.Fatal(err)
		}
		if !b.set || !b.value {
			t.Fatalf("Set(true): set=%v value=%v, want set true value true", b.set, b.value)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		var b tristateBool
		if err := b.Set("maybe"); err == nil {
			t.Fatal("expected invalid bool error")
		}
	})
}

func TestResolveForkCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		source   bool
		override tristateBool
		want     bool
	}{
		{name: "inherit_disabled", want: false},
		{name: "inherit_enabled", source: true, want: true},
		{name: "enable", override: tristateBool{set: true, value: true}, want: true},
		{name: "disable_source", source: true, override: tristateBool{set: true, value: false}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.override.resolveForkCapability(tt.source)
			if got != tt.want {
				t.Fatalf("resolveForkCapability = %v, want %v", got, tt.want)
			}
		})
	}
}

func runMainTestGit(t *testing.T, wd string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args are from test code.
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "LANG=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
}

func TestSplitBranches(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got, err := splitBranches("main,feature")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"main", "feature"}) {
			t.Fatalf("splitBranches = %v, want [main feature]", got)
		}
		got, err = splitBranches("")
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("splitBranches empty = %v, want nil", got)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"main,", ",main", "main,,feature", "main, feature", "main ,feature"} {
			if _, err := splitBranches(in); err == nil {
				t.Fatalf("splitBranches(%q) error = nil, want error", in)
			}
		}
	})
}

func TestAllocateForkBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	gitRoot := t.TempDir()
	runMainTestGit(t, gitRoot, "init", "-q", "--initial-branch=main")
	runMainTestGit(t, gitRoot, "config", "user.name", "Test")
	runMainTestGit(t, gitRoot, "config", "user.email", "test@test")
	if err := os.WriteFile(filepath.Join(gitRoot, "README.md"), []byte("main\n"), 0o644); err != nil { //nolint:gosec // test data.
		t.Fatal(err)
	}
	runMainTestGit(t, gitRoot, "add", ".")
	runMainTestGit(t, gitRoot, "commit", "-q", "-m", "main")

	// main-0 is taken by an existing container mapping the same repo, so the next
	// free name is main-1. A container mapping a different repo is ignored.
	existing := []*md.Container{
		{Repos: []md.Repo{{GitRoot: gitRoot, Branches: []string{"other", "main-0"}}}},
		{Repos: []md.Repo{{GitRoot: t.TempDir(), Branches: []string{"main-1"}}}},
	}
	if got, err := allocateForkBranch(ctx, gitRoot, "main", existing); err != nil {
		t.Fatal(err)
	} else if got != "main-1" {
		t.Fatalf("allocateForkBranch = %q, want main-1", got)
	}

	// With no existing containers, allocation starts at main-0.
	if got, err := allocateForkBranch(ctx, gitRoot, "main", nil); err != nil {
		t.Fatal(err)
	} else if got != "main-0" {
		t.Fatalf("allocateForkBranch = %q, want main-0", got)
	}

	// A local ref also blocks a candidate: after creating main-0, allocation
	// skips to main-1.
	runMainTestGit(t, gitRoot, "branch", "main-0")
	if got, err := allocateForkBranch(ctx, gitRoot, "main", nil); err != nil {
		t.Fatal(err)
	} else if got != "main-1" {
		t.Fatalf("allocateForkBranch = %q, want main-1", got)
	}
}

func TestValidateBranchesExist(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	runMainTestGit(t, dir, "init", "-q", "--initial-branch=main")
	runMainTestGit(t, dir, "config", "user.name", "Test")
	runMainTestGit(t, dir, "config", "user.email", "test@test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("main\n"), 0o644); err != nil { //nolint:gosec // test data, world-readable is fine.
		t.Fatal(err)
	}
	runMainTestGit(t, dir, "add", ".")
	runMainTestGit(t, dir, "commit", "-q", "-m", "main")
	runMainTestGit(t, dir, "checkout", "-q", "-b", "feature")
	g, err := git.RootDir(ctx, dir, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBranchesExist(ctx, g, []string{"main", "feature"}, "repo "+dir); err != nil {
		t.Fatal(err)
	}
	if err := validateBranchesExist(ctx, g, []string{"missing"}, "repo "+dir); err == nil {
		t.Fatal("validateBranchesExist error = nil, want missing branch error")
	}
}

func TestNoMatchingContainerError(t *testing.T) {
	t.Parallel()

	t.Run("no_repository_mapping_suggests_start", func(t *testing.T) {
		t.Parallel()
		err := noMatchingContainerError("/src/project with spaces", "feature-$(touch-pwn)", true, "diff", []*md.Container{
			{Repos: []md.Repo{{GitRoot: "/src/other", Branches: []string{"main"}}}},
		})
		for _, want := range []string{
			`no container found for repository "/src/project with spaces" and branch "feature-$(touch-pwn)"`,
			`md start '-repo=/src/project with spaces' '-branch=feature-$(touch-pwn)'`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})

	t.Run("missing_explicit_branch_requires_a_choice", func(t *testing.T) {
		t.Parallel()
		err := noMatchingContainerError("/src/project", "typo", false, "diff", nil)
		for _, want := range []string{
			`local branch "typo" does not exist`,
			"create it or choose an existing branch before starting a container",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "md start") {
			t.Fatalf("error suggests a start command that cannot work: %v", err)
		}
	})

	t.Run("other_mapped_branches_suggest_diff_choices", func(t *testing.T) {
		t.Parallel()
		err := noMatchingContainerError("/src/project with spaces", "missing", false, "diff", []*md.Container{
			{Repos: []md.Repo{{GitRoot: "/src/project with spaces", Branches: []string{"main", "feature-$(touch-pwn)"}}}},
			{Repos: []md.Repo{{GitRoot: "/src/project with spaces", Branches: []string{"main"}}}},
		})
		for _, want := range []string{
			`no container found for repository "/src/project with spaces" and branch "missing"`,
			`containers exist for other mapped branches`,
			`md diff '-repo=/src/project with spaces' '-branch=feature-$(touch-pwn)'`,
			`md diff '-repo=/src/project with spaces' -branch=main`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		if strings.Count(err.Error(), "-branch=main") != 1 {
			t.Fatalf("error repeats mapped branch choice: %v", err)
		}
	})
}

func TestResolveCaches(t *testing.T) {
	t.Parallel()
	allNames := func(caches []md.CacheMount) []string {
		names := make([]string, len(caches))
		for i, c := range caches {
			names[i] = c.Name
		}
		return names
	}

	t.Run("default_includes_all_well_known", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches(nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		// Must be non-nil (not nil) so imageBuildNeeded always checks the key.
		if got == nil {
			t.Fatal("expected non-nil slice")
		}
		// Every well-known cache must appear.
		present := make(map[string]bool)
		for _, c := range got {
			present[c.Name] = true
		}
		for name, mounts := range md.WellKnownCaches {
			for _, m := range mounts {
				if !present[m.Name] {
					t.Errorf("well-known cache %q (%s) missing from default result", name, m.Name)
				}
			}
		}
	})

	t.Run("no_caches_returns_empty_non_nil", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches(nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("expected non-nil slice")
		}
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", allNames(got))
		}
	})

	t.Run("no_cache_excludes_named", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches(nil, []string{"go-mod"}, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			if c.Name == "go-mod" {
				t.Error("go-mod should have been excluded")
			}
		}
		// Other well-known caches should still be present.
		present := make(map[string]bool)
		for _, c := range got {
			present[c.Name] = true
		}
		for name, mounts := range md.WellKnownCaches {
			if name == "go-mod" {
				continue
			}
			for _, m := range mounts {
				if !present[m.Name] {
					t.Errorf("cache %q unexpectedly absent", m.Name)
				}
			}
		}
	})

	t.Run("no_cache_unknown_name_errors", func(t *testing.T) {
		t.Parallel()
		_, err := resolveCaches(nil, []string{"nonexistent"}, false)
		if err == nil {
			t.Fatal("expected error for unknown --no-cache name")
		}
	})

	t.Run("custom_cache_added", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"/host/path:/container/path"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "path" || got[0].HostPath != "/host/path" || got[0].ContainerPath != "/container/path" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("custom_cache_name_sanitized", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"/host/Foo.Bar_cache:/container/path"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "foo-bar-cache" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("custom_cache_expands_container_tilde", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"~/.cache/tool:~/.cache/tool"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostPath != "~/.cache/tool" || got[0].ContainerPath != "/home/user/.cache/tool" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("custom_cache_expands_bare_container_tilde", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"~:~"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "home" || got[0].HostPath != "~" || got[0].ContainerPath != "/home/user" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("no_caches_plus_cache_readds_well_known", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"go-mod"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		present := make(map[string]bool)
		for _, c := range got {
			present[c.Name] = true
		}
		for _, m := range md.WellKnownCaches["go-mod"] {
			if !present[m.Name] {
				t.Errorf("go-mod cache %q should have been re-added", m.Name)
			}
		}
		// No other well-known caches.
		for name, mounts := range md.WellKnownCaches {
			if name == "go-mod" {
				continue
			}
			for _, m := range mounts {
				if present[m.Name] {
					t.Errorf("cache %q should not be present", m.Name)
				}
			}
		}
	})

	t.Run("no_duplicate_when_cache_already_default", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"go-mod"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, c := range got {
			if c.Name == "go-mod" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected go-mod exactly once, got %d", count)
		}
	})

	t.Run("custom_cache_ro", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"/host:/cnt:ro"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got[0].ReadOnly {
			t.Errorf("expected read-only cache, got %+v", got)
		}
	})

	t.Run("invalid_custom_spec_errors", func(t *testing.T) {
		t.Parallel()
		_, err := resolveCaches([]string{"notapath"}, nil, true)
		if err == nil {
			t.Fatal("expected error for invalid custom spec")
		}
	})
}

func TestResolveMounts(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got, err := resolveMounts([]string{"/host/path:/container/path"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostPath != "/host/path" || got[0].ContainerPath != "/container/path" || got[0].ReadOnly {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("readonly", func(t *testing.T) {
		t.Parallel()
		got, err := resolveMounts([]string{"/host/path:/container/path:ro"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got[0].ReadOnly {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("container_tilde", func(t *testing.T) {
		t.Parallel()
		got, err := resolveMounts([]string{"~/host:~/container"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostPath != "~/host" || got[0].ContainerPath != "/home/user/container" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, spec := range []string{"notapath", "/host:/container:rw"} {
			if _, err := resolveMounts([]string{spec}); err == nil {
				t.Errorf("resolveMounts(%q): expected error", spec)
			}
		}
	})
}

func TestResolveEnvSpecs(t *testing.T) {
	t.Parallel()
	lookup := func(name string) (string, bool) {
		values := map[string]string{
			"FROM_HOST":  "host value",
			"HOST_EMPTY": "",
			"HOST_MULTI": "host line 1\nhost line 2",
			"QUOTE":      "a'b",
		}
		value, ok := values[name]
		return value, ok
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got, err := resolveEnvSpecs([]string{"FROM_HOST", "HOST_EMPTY", "HOST_MULTI", "LITERAL=hello world", "MULTI=line 1\nline 2", "QUOTE", "UNSET="}, lookup)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"FROM_HOST=host value", "HOST_EMPTY=", "HOST_MULTI=host line 1\nhost line 2", "LITERAL=hello world", "MULTI=line 1\nline 2", "QUOTE=a'b", "UNSET="}
		if !slices.Equal(got, want) {
			t.Errorf("resolveEnvSpecs() = %v, want %v", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, spec := range []string{"", "1BAD=x", "BAD-NAME=x", "MISSING"} {
			if _, err := resolveEnvSpecs([]string{spec}, lookup); err == nil {
				t.Errorf("resolveEnvSpecs(%q): expected error", spec)
			}
		}
	})
}

func TestShellSplit(t *testing.T) {
	t.Parallel()
	t.Run("simple", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("--memory 4g")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--memory", "4g"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("single_arg", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("--privileged")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "--privileged" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("equals_form", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("--memory=4g")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "--memory=4g" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("single_quotes", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("-v '/path with spaces:/container'")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "-v" || got[1] != "/path with spaces:/container" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("double_quotes", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit(`-e "FOO=hello world"`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "-e" || got[1] != "FOO=hello world" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("backslash_escape", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit(`--label key=val\ ue`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "--label" || got[1] != "key=val ue" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("unterminated_single_quote", func(t *testing.T) {
		t.Parallel()
		_, err := shellSplit("--flag 'oops")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("unterminated_double_quote", func(t *testing.T) {
		t.Parallel()
		_, err := shellSplit(`--flag "oops`)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
