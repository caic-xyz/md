// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for client.go

package md

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	fakeRuntimeEnv          = "MD_TEST_FAKE_RUNTIME"
	fakeRuntimeLocalBaseEnv = "MD_TEST_FAKE_RUNTIME_LOCAL_BASE"
	fakeRuntimeLogEnv       = "MD_TEST_FAKE_RUNTIME_LOG"
	fakeSSHEnv              = "MD_TEST_FAKE_SSH"
	fakeSSHLogEnv           = "MD_TEST_FAKE_SSH_LOG"
)

func testLogger(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testLogWriter struct {
	t testing.TB
}

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func TestMain(m *testing.M) {
	if os.Getenv(fakeSSHEnv) == "1" && isFakeSSHExecutable(os.Args[0]) {
		os.Exit(runFakeSSH(os.Args[1:]))
	}
	if os.Getenv(fakeRuntimeEnv) == "1" {
		os.Exit(runFakeRuntime(os.Args[1:], os.Getenv(fakeRuntimeLogEnv), os.Getenv(fakeRuntimeLocalBaseEnv) == "1"))
	}
	os.Exit(m.Run())
}

func isFakeSSHExecutable(name string) bool {
	base := filepath.Base(name)
	return base == "ssh" || base == "ssh.exe"
}

func TestSanitizeDockerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "simple", "simple"},
		{"slash", "with/slash", "with-slash"},
		{"at", "with@at", "with-at"},
		{"feature_branch", "feature/my-branch", "feature-my-branch"},
		{"double_slash", "a//b", "a-b"},
		{"only_dashes", "---", "unnamed"},
		{"empty", "", "unnamed"},
		{"bang", "hello world!", "helloworld"},
		{"leading_dot", ".leading", "leading"},
		{"trailing_dot", "trailing.", "trailing"},
		{"collapse", "a--b..c__d", "a-b-c-d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeDockerName(tt.in); got != tt.want {
				t.Errorf("sanitizeDockerName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestContainerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		repo, branch string
		want         string
	}{
		{"simple", "myrepo", "main", "md-myrepo-main"},
		{"slashes", "my/repo", "feature/branch", "md-my-repo-feature-branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containerName(tt.repo, tt.branch); got != tt.want {
				t.Errorf("containerName(%q, %q) = %q, want %q", tt.repo, tt.branch, got, tt.want)
			}
		})
	}
}

func TestHarnessMounts(t *testing.T) {
	t.Parallel()
	if len(HarnessMounts) == 0 {
		t.Fatal("HarnessMounts must not be empty")
	}
	for harness, paths := range HarnessMounts {
		if paths.Description == "" {
			t.Errorf("HarnessMounts[%q]: Description is empty", harness)
		}
	}
}

func TestEnvWithOverrides(t *testing.T) {
	t.Parallel()
	got := envWithOverrides(
		[]string{"HOME=/real", "PATH=/bin", "KEEP=1"},
		[]string{"HOME=/tmp/md", "XDG_CONFIG_HOME=/tmp/md/.config"},
	)

	counts := map[string]int{}
	values := map[string]string{}
	for _, kv := range got {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		counts[k]++
		values[k] = v
	}
	if counts["HOME"] != 1 {
		t.Fatalf("HOME count = %d, want 1 in %v", counts["HOME"], got)
	}
	if values["HOME"] != "/tmp/md" {
		t.Fatalf("HOME = %q, want /tmp/md", values["HOME"])
	}
	if values["PATH"] != "/bin" {
		t.Fatalf("PATH = %q, want /bin", values["PATH"])
	}
	if values["XDG_CONFIG_HOME"] != "/tmp/md/.config" {
		t.Fatalf("XDG_CONFIG_HOME = %q, want /tmp/md/.config", values["XDG_CONFIG_HOME"])
	}
}

func TestWellKnownCaches(t *testing.T) {
	t.Parallel()
	if len(WellKnownCaches) == 0 {
		t.Fatal("WellKnownCaches must not be empty")
	}
	for name, mounts := range WellKnownCaches {
		if len(mounts) == 0 {
			t.Errorf("WellKnownCaches[%q] is empty", name)
		}
		for _, m := range mounts {
			if m.Name == "" {
				t.Errorf("WellKnownCaches[%q]: CacheMount.Name is empty", name)
			}
			if !strings.HasPrefix(m.HostPath, "~/") {
				t.Errorf("WellKnownCaches[%q] %q: HostPath should start with ~/; got %q", name, m.Name, m.HostPath)
			}
			if !strings.HasPrefix(m.ContainerPath, "/home/user/") {
				t.Errorf("WellKnownCaches[%q] %q: ContainerPath should start with /home/user/; got %q", name, m.Name, m.ContainerPath)
			}
		}
		if mounts[0].Description == "" {
			t.Errorf("WellKnownCaches[%q]: first CacheMount.Description is empty", name)
		}
	}
}

func TestClient(t *testing.T) {
	t.Parallel()
	t.Run("Logger", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(io.MultiWriter(&buf, testLogWriter{t: t}), &slog.HandlerOptions{Level: slog.LevelDebug}))
		c := &Client{Logger: logger}
		c.Logger.Log(t.Context(), slog.LevelDebug, "client")
		ct := &Container{Client: c, Name: "md-test"}
		ct.Logger.Log(t.Context(), slog.LevelDebug, "container")

		got := buf.String()
		for _, want := range []string{`msg=client`, `msg=container`} {
			if !strings.Contains(got, want) {
				t.Fatalf("log output = %q, want %q", got, want)
			}
		}
	})
	t.Run("CommandLogsRedactSensitiveValues", func(t *testing.T) {
		t.Parallel()
		rt, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		logger := slog.New(slog.NewTextHandler(io.MultiWriter(&log, testLogWriter{t: t}), &slog.HandlerOptions{Level: slog.LevelDebug}))
		c := &Client{
			Logger:  logger,
			Runtime: rt,
			env: []string{
				fakeRuntimeEnv + "=1",
				fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			},
		}
		args := []string{
			rt, "build",
			"-e", "TAILSCALE_AUTHKEY=tskey-secret",
			"--env=MD_SUDO_PASSWORD=sudo-secret",
			"--env", "OPENAI_API_KEY=api-key-secret",
			"--build-arg", "API_KEY_FILE=api-key-file-secret",
			"--other=API_KEY_CONFIG=api-key-config-secret",
			"--label", "md.sudo-password=label-secret",
			"TailscaleAPIKey=camel-api-secret",
			"--password", "flag-secret",
			"--label", "md.display=1",
		}
		if _, err := c.runCmd(t.Context(), "", args); err != nil {
			t.Fatal(err)
		}
		if err := c.runCmdOut(t.Context(), "", args, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}

		got := log.String()
		for _, secret := range []string{"tskey-secret", "sudo-secret", "api-key-secret", "api-key-file-secret", "api-key-config-secret", "label-secret", "camel-api-secret", "flag-secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("log output leaked %q:\n%s", secret, got)
			}
		}
		for _, want := range []string{"TAILSCALE_AUTHKEY=<redacted>", "MD_SUDO_PASSWORD=<redacted>", "OPENAI_API_KEY=<redacted>", "API_KEY_FILE=<redacted>", "--other=API_KEY_CONFIG=<redacted>", "md.sudo-password=<redacted>", "TailscaleAPIKey=<redacted>", "--password", "<redacted>", "md.display=1"} {
			if !strings.Contains(got, want) {
				t.Fatalf("log output = %q, want %q", got, want)
			}
		}
	})
	t.Run("WatchDieEvents", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				in   string
				want ContainerEvent
			}{
				{
					name: "docker",
					in:   `{"Actor":{"Attributes":{"name":"md-docker","image":"img"}}}`,
					want: ContainerEvent{Name: "md-docker", Attributes: map[string]string{"image": "img"}},
				},
				{
					name: "podman",
					in:   `{"Name":"md-podman","Attributes":{"image":"img"}}`,
					want: ContainerEvent{Name: "md-podman", Attributes: map[string]string{"image": "img"}},
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					ev, ok := parseContainerEvent([]byte(tt.in))
					if !ok {
						t.Fatal("parseContainerEvent returned ok=false")
					}
					if ev.Name != tt.want.Name || ev.Attributes["image"] != tt.want.Attributes["image"] {
						t.Fatalf("event = %+v, want %+v", ev, tt.want)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if _, ok := parseContainerEvent([]byte(`{"Attributes":{"image":"img"}}`)); ok {
				t.Fatal("parseContainerEvent ok=true, want false")
			}
		})
	})
	t.Run("WatchStats", func(t *testing.T) {
		t.Parallel()
		rt, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		c := &Client{
			Logger:  testLogger(t),
			Runtime: rt,
			env: []string{
				fakeRuntimeEnv + "=1",
				fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
				fakeRuntimeLocalBaseEnv + "=0",
			},
		}
		stats, err := c.WatchStats(t.Context(), []string{"md-one"})
		if err != nil {
			t.Fatal(err)
		}
		var got []ContainerStatsSample
		for s, err := range stats {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		if len(got) != 1 {
			t.Fatalf("stats len = %d, want 1", len(got))
		}
		if got[0].Name != "md-one" || got[0].Stats.CPUPerc != 1.5 || got[0].Stats.PIDs != 3 {
			t.Fatalf("stats = %+v", got[0])
		}
		if got[0].Stats.DiskUsed != -1 {
			t.Fatalf("DiskUsed = %d, want -1", got[0].Stats.DiskUsed)
		}
	})
	t.Run("AgentMounts", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		c, err := newClient(home, "docker", io.Discard)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		c.Logger = testLogger(t)
		mounts, err := c.AgentMounts(HarnessMounts[HarnessClaude])
		if err != nil {
			t.Fatalf("AgentMounts: %v", err)
		}
		wantDirs := []string{
			filepath.Join(home, ".claude"),
			filepath.Join(home, ".config", "agents"),
			filepath.Join(home, ".config", "md"),
		}
		for _, d := range wantDirs {
			info, err := os.Stat(d)
			if err != nil {
				t.Fatalf("stat %s: %v", d, err)
			}
			if !info.IsDir() {
				t.Fatalf("%s is not a directory", d)
			}
		}
		if !slices.ContainsFunc(mounts, func(m Mount) bool {
			return m.HostPath == filepath.Join(home, ".claude") && m.ContainerPath == "/home/user/.claude"
		}) {
			t.Fatalf("AgentMounts missing Claude mount: %+v", mounts)
		}
		if !slices.ContainsFunc(mounts, func(m Mount) bool {
			return m.HostPath == filepath.Join(home, ".config", "md") && m.ContainerPath == "/home/user/.config/md" && m.ReadOnly
		}) {
			t.Fatalf("AgentMounts missing read-only md mount: %+v", mounts)
		}
		target, err := os.Readlink(filepath.Join(home, ".claude.json"))
		if err != nil {
			t.Fatalf("Readlink .claude.json: %v", err)
		}
		if target != filepath.Join(home, ".claude", "claude.json") {
			t.Fatalf(".claude.json target = %q, want %q", target, filepath.Join(home, ".claude", "claude.json"))
		}
	})
	t.Run("Container", func(t *testing.T) {
		t.Parallel()
		c := &Client{Logger: testLogger(t)}
		tests := []struct {
			name     string
			gitRoot  string
			wantRepo string
			wantName string
		}{
			{"regular", "/home/user/src/myrepo", "/home/user/src/myrepo", "md-myrepo-main"},
			{"bare", "/home/user/src/myrepo.git", "/home/user/src/myrepo", "md-myrepo-main"},
			{"no_git_suffix", "/home/user/src/myrepo.git.git", "/home/user/src/myrepo.git", "md-myrepo.git-main"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				ct, err := c.Container(Repo{GitRoot: tt.gitRoot, Branch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				if ct.Repos[0].MountedPath != tt.wantRepo {
					t.Errorf("MountedPath = %q, want %q", ct.Repos[0].MountedPath, tt.wantRepo)
				}
				if ct.Name != tt.wantName {
					t.Errorf("Name = %q, want %q", ct.Name, tt.wantName)
				}
			})
		}
	})
	t.Run("Runtime", func(t *testing.T) {
		t.Parallel()
		t.Run("new_defaults_to_docker", func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			c, err := newClient(tmp, "", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			c.Logger = testLogger(t)
			if c.Runtime == "" {
				t.Error("New() should set Runtime")
			}
		})
		t.Run("explicit", func(t *testing.T) {
			t.Parallel()
			c := &Client{Logger: testLogger(t), Runtime: "podman"}
			if c.Runtime != "podman" {
				t.Errorf("Runtime = %q, want %q", c.Runtime, "podman")
			}
		})
	})
}

func TestDetectRuntime(t *testing.T) {
	t.Parallel()
	t.Run("fallback_to_docker", func(t *testing.T) {
		t.Parallel()
		lookPath := func(name string) (string, error) {
			return "", exec.ErrNotFound
		}
		if got := detectRuntime(lookPath); got != "docker" {
			t.Errorf("detectRuntime() = %q, want %q (fallback)", got, "docker")
		}
	})
	t.Run("finds_podman_when_no_docker", func(t *testing.T) {
		t.Parallel()
		lookPath := func(name string) (string, error) {
			if name == "podman" {
				return "/usr/bin/podman", nil
			}
			return "", exec.ErrNotFound
		}
		if got := detectRuntime(lookPath); got != "podman" {
			t.Errorf("detectRuntime() = %q, want %q", got, "podman")
		}
	})
}

func TestBuildSpecializedImage(t *testing.T) { //nolint:paralleltest // fakeRuntime uses t.Setenv, which cannot run in parallel tests.
	t.Run("uses_local_remote_base_when_pull_fails", func(t *testing.T) { //nolint:paralleltest // fakeRuntime uses t.Setenv, which cannot run in parallel tests.
		home := t.TempDir()
		logPath := filepath.Join(home, "runtime.log")
		c := newTestClient(t, home, fakeRuntime(t, logPath, true))
		var stdout strings.Builder
		var stderr strings.Builder

		if err := c.buildSpecializedImage(t.Context(), &stdout, &stderr, "md-specialized-test", "ghcr.io/caic-xyz/md-user:latest", PlatformLinuxAMD64.String(), nil, nil, false); err != nil {
			t.Fatalf("buildSpecializedImage: %v", err)
		}
		if !strings.Contains(stdout.String(), "using local copy") {
			t.Fatalf("stdout = %q, want offline fallback warning", stdout.String())
		}
		logData, err := os.ReadFile(logPath) //nolint:gosec // path is from t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		log := string(logData)
		if !strings.Contains(log, "pull --platform linux/amd64 ghcr.io/caic-xyz/md-user:latest") {
			t.Fatalf("runtime log missing pull command:\n%s", log)
		}
		if !strings.Contains(log, "build --no-cache --platform linux/amd64 -t md-specialized-test") {
			t.Fatalf("runtime log missing build command:\n%s", log)
		}
		if strings.Contains(log, "manifest inspect") {
			t.Fatalf("runtime log should not inspect remote manifest after pull failure:\n%s", log)
		}
	})

	t.Run("fails_when_pull_fails_without_local_base", func(t *testing.T) { //nolint:paralleltest // fakeRuntime uses t.Setenv, which cannot run in parallel tests.
		home := t.TempDir()
		logPath := filepath.Join(home, "runtime.log")
		c := newTestClient(t, home, fakeRuntime(t, logPath, false))

		err := c.buildSpecializedImage(t.Context(), io.Discard, io.Discard, "md-specialized-test", "ghcr.io/caic-xyz/md-user:latest", PlatformLinuxAMD64.String(), nil, nil, false)
		if err == nil {
			t.Fatal("buildSpecializedImage succeeded, want pull failure")
		}
		if !strings.Contains(err.Error(), "pulling base image") {
			t.Fatalf("err = %v, want pull failure", err)
		}
	})
}

func newTestClient(t *testing.T, home, runtimePath string) *Client {
	c := &Client{
		Home:          home,
		XDGConfigHome: filepath.Join(home, ".config"),
		XDGDataHome:   filepath.Join(home, ".local", "share"),
		XDGStateHome:  filepath.Join(home, ".local", "state"),
		HostKeyPath:   filepath.Join(home, ".config", "md", "ssh_host_ed25519_key"),
		UserKeyPath:   filepath.Join(home, ".ssh", "md"),
		Runtime:       runtimePath,
		Logger:        testLogger(t),
	}
	c.keysDir = filepath.Join(c.XDGConfigHome, "md")
	if err := c.setupSSH(io.Discard); err != nil {
		t.Fatal(err)
	}
	return c
}

func fakeRuntime(t *testing.T, logPath string, localBase bool) string {
	t.Setenv(fakeRuntimeEnv, "1")
	t.Setenv(fakeRuntimeLogEnv, logPath)
	if localBase {
		t.Setenv(fakeRuntimeLocalBaseEnv, "1")
	} else {
		t.Setenv(fakeRuntimeLocalBaseEnv, "0")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func fakeSSH(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Cleanup(func() {
		// Windows can keep a just-executed fake ssh.exe locked briefly.
		if err := removeAllWithRetry(dir); err != nil {
			t.Errorf("removing fake SSH dir %q: %v", dir, err)
		}
	})
	name := "ssh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fakePath := filepath.Join(dir, name)
	if err := linkOrCopyExecutable(exe, fakePath); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeSSHEnv, "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func removeAllWithRetry(path string) error {
	const attempts = 100
	var err error
	for range attempts {
		err = os.RemoveAll(path)
		if err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

func linkOrCopyExecutable(src, dst string) error {
	// A Windows hardlink to the running test binary stays locked by this process.
	if runtime.GOOS != "windows" {
		if err := os.Link(src, dst); err == nil {
			return nil
		}
	}
	in, err := os.Open(src) //nolint:gosec // test helper copies the current test binary.
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // fake executable in private temp dir must be executable.
	if err != nil {
		return errors.Join(err, in.Close())
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close(), in.Close())
}

func runFakeSSH(args []string) int {
	if logPath := os.Getenv(fakeSSHLogEnv); logPath != "" {
		if err := appendFakeCommandLog(logPath, args); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "writing fake ssh log: %v\n", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hostIndex := 0
	for hostIndex < len(args) && strings.HasPrefix(args[hostIndex], "-") {
		switch args[hostIndex] {
		case "-F", "-i", "-o", "-p":
			hostIndex += 2
		default:
			hostIndex++
		}
	}
	if hostIndex >= len(args) {
		_, _ = fmt.Fprintln(os.Stderr, "missing ssh host")
		return 1
	}
	if hostIndex+1 >= len(args) {
		return 0
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", strings.Join(args[hostIndex+1:], " ")) //nolint:gosec // test fake executes trusted test commands.
	cmd.Env = append(os.Environ(), "LANG=C")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(os.Stderr, "fake ssh: %v\n", err)
		return 1
	}
	return 0
}

func runFakeRuntime(args []string, logPath string, localBase bool) int {
	if err := appendFakeCommandLog(logPath, args); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "writing fake runtime log: %v\n", err)
		return 1
	}
	if len(args) >= 2 && args[0] == "inspect" {
		return fakeRuntimeContainerInspect(args)
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return fakeRuntimeInspect(args, localBase)
	}
	if len(args) >= 1 && args[0] == "pull" {
		_, _ = fmt.Fprintln(os.Stderr, "network unreachable")
		return 1
	}
	if len(args) >= 1 && args[0] == "build" {
		return 0
	}
	if len(args) >= 1 && args[0] == "stats" {
		_, _ = fmt.Fprintln(os.Stdout, `{"Name":"md-one","CPUPerc":"1.5%","MemUsage":"10MiB / 1GiB","MemPerc":"1%","PIDs":"3","NetIO":"2kB / 3kB","BlockIO":"4kB / 5kB"}`)
		return 0
	}
	if len(args) >= 2 && args[0] == "manifest" && args[1] == "inspect" {
		_, _ = fmt.Fprintln(os.Stdout, `{"manifests":[{"digest":"sha256:remote","platform":{"architecture":"amd64","os":"linux"}}]}`)
		return 0
	}
	if len(args) >= 1 && args[0] == "builder" {
		return 0
	}
	_, _ = fmt.Fprintf(os.Stderr, "unexpected command: %s\n", strings.Join(args, " "))
	return 1
}

func fakeRuntimeContainerInspect(args []string) int {
	if len(args) == 4 && args[1] == "md-test" && args[2] == "--format" && args[3] == "{{.Os}}/{{.Architecture}}" {
		_, _ = fmt.Fprintln(os.Stdout, "linux/amd64")
		return 0
	}
	if len(args) == 2 && args[1] == "md-test" {
		_, _ = fmt.Fprintln(os.Stdout, `[{"Name":"/md-test","Id":"ctr","Image":"sha256:image","Platform":"linux","Config":{"Image":"base:latest","Labels":{}},"State":{"Status":"running"}}]`)
		return 0
	}
	_, _ = fmt.Fprintf(os.Stderr, "unexpected inspect command: %s\n", strings.Join(args, " "))
	return 1
}

func appendFakeCommandLog(logPath string, args []string) error {
	if logPath == "" {
		return errors.New("missing fake runtime log path")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test log path comes from t.TempDir.
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, strings.Join(args, " ")); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func fakeRuntimeInspect(args []string, localBase bool) int {
	argLine := strings.Join(args, " ")
	if strings.Contains(argLine, "{{.Id}}") {
		if localBase {
			_, _ = fmt.Fprintln(os.Stdout, "sha256:local")
			return 0
		}
		return 1
	}
	if strings.Contains(argLine, "{{index .RepoDigests 0}}") {
		if localBase {
			_, _ = fmt.Fprintln(os.Stdout, "ghcr.io/caic-xyz/md-user@sha256:local")
			return 0
		}
		return 1
	}
	if strings.Contains(argLine, "md.version") {
		return 0
	}
	_, _ = fmt.Fprintf(os.Stderr, "unexpected image inspect command: %s\n", argLine)
	return 1
}

func TestBaseImageIsLocal(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, image := range []string{
			"md-user-local",
			"md-user-local:latest",
			"md-user-local@sha256:0123456789abcdef",
			"ubuntu:latest",
			"myteam/image:latest",
		} {
			c := &Client{Logger: testLogger(t), Runtime: "true"}
			if !c.baseImageIsLocal(t.Context(), image) {
				t.Errorf("baseImageIsLocal(%q) = false, want true", image)
			}
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		c := &Client{Logger: testLogger(t), Runtime: "false"}
		for _, image := range []string{"ubuntu:latest", "md-user-local:latest", "myteam/image:latest"} {
			if c.baseImageIsLocal(t.Context(), image) {
				t.Errorf("baseImageIsLocal(%q) = true, want false", image)
			}
		}
		c = &Client{Logger: testLogger(t), Runtime: "true"}
		for _, image := range []string{"docker.io/library/ubuntu:latest", "ghcr.io/caic-xyz/md-user:latest", "localhost:5000/md-user:latest"} {
			if c.baseImageIsLocal(t.Context(), image) {
				t.Errorf("baseImageIsLocal(%q) = true, want false", image)
			}
		}
	})
}

func TestHasExplicitRegistry(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, image := range []string{"docker.io/library/ubuntu:latest", "ghcr.io/caic-xyz/md-user:latest", "localhost/md-user:latest", "localhost:5000/md-user:latest"} {
			if !hasExplicitRegistry(image) {
				t.Errorf("hasExplicitRegistry(%q) = false, want true", image)
			}
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, image := range []string{"ubuntu:latest", "md-user-local:latest", "myteam/image:latest", "ubuntu@sha256:0123456789abcdef"} {
			if hasExplicitRegistry(image) {
				t.Errorf("hasExplicitRegistry(%q) = true, want false", image)
			}
		}
	})
}

func TestIsRootlessPodman(t *testing.T) {
	t.Parallel()
	t.Run("docker", func(t *testing.T) {
		t.Parallel()
		if isRootlessPodman("docker") {
			t.Error("isRootlessPodman(\"docker\") = true, want false")
		}
	})
	t.Run("podman", func(t *testing.T) {
		t.Parallel()
		got := isRootlessPodman("podman")
		if runtime.GOOS == "linux" {
			// On Linux, result depends on whether tests run as root.
			want := os.Getuid() != 0
			if got != want {
				t.Errorf("isRootlessPodman(\"podman\") = %v, want %v (uid=%d)", got, want, os.Getuid())
			}
		} else if got {
			t.Error("isRootlessPodman(\"podman\") = true on non-Linux, want false")
		}
	})
}

func TestRscFS(t *testing.T) {
	t.Parallel()
	t.Run("root_Dockerfile", func(t *testing.T) {
		t.Parallel()
		if _, err := rscFS.ReadFile("rsc/root/Dockerfile"); err != nil {
			t.Fatalf("embedded rsc/root/Dockerfile not found: %v", err)
		}
	})
	t.Run("user_Dockerfile", func(t *testing.T) {
		t.Parallel()
		if _, err := rscFS.ReadFile("rsc/user/Dockerfile"); err != nil {
			t.Fatalf("embedded rsc/user/Dockerfile not found: %v", err)
		}
	})
}
