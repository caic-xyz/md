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
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/caic-xyz/md/containers"
)

const (
	fakeRuntimeEnv          = "MD_TEST_FAKE_RUNTIME"
	fakeRuntimeErrorEnv     = "MD_TEST_FAKE_RUNTIME_ERROR"
	fakeRuntimeStateEnv     = "MD_TEST_FAKE_RUNTIME_STATE"
	fakeRuntimeLocalBaseEnv = "MD_TEST_FAKE_RUNTIME_LOCAL_BASE"
	fakeRuntimeLogEnv       = "MD_TEST_FAKE_RUNTIME_LOG"
	fakeGitDiffFailureEnv   = "MD_TEST_FAKE_GIT_DIFF_FAILURE"
	fakeGitExecutableEnv    = "MD_TEST_FAKE_GIT_EXECUTABLE"
	fakeGitMarkerEnv        = "MD_TEST_FAKE_GIT_MARKER"
	fakeGitVanishPathEnv    = "MD_TEST_FAKE_GIT_VANISH_PATH"
	fakeSSHEnv              = "MD_TEST_FAKE_SSH"
	fakeSSHFailureMatchEnv  = "MD_TEST_FAKE_SSH_FAILURE_MATCH"
	fakeSSHFailureStatusEnv = "MD_TEST_FAKE_SSH_FAILURE_STATUS"
	fakeSSHFailureTextEnv   = "MD_TEST_FAKE_SSH_FAILURE_TEXT"
	fakeSSHLogEnv           = "MD_TEST_FAKE_SSH_LOG"
)

func testRuntime(t testing.TB, name string, logger *slog.Logger, env []string) containers.Runtime {
	r, err := containers.New(name, logger, env)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testRunningRuntime(t testing.TB, logger *slog.Logger) containers.Runtime {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		fakeRuntimeEnv + "=1",
		fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		fakeRuntimeStateEnv + "=running",
	}
	return testRuntime(t, executable, logger, env)
}

func testClient(t testing.TB) *Client {
	logger := testLogger(t)
	return &Client{Logger: logger, Runtime: testRuntime(t, "docker", logger, nil)}
}

func TestMain(m *testing.M) {
	if os.Getenv(fakeGitDiffFailureEnv) == "1" && isFakeGitExecutable(os.Args[0]) {
		os.Exit(runFakeGit(os.Args[1:]))
	}
	if os.Getenv(fakeSSHEnv) == "1" && isFakeSSHExecutable(os.Args[0]) {
		os.Exit(runFakeSSH(os.Args[1:]))
	}
	if os.Getenv(fakeRuntimeEnv) == "1" {
		os.Exit(runFakeRuntime(os.Args[1:], os.Getenv(fakeRuntimeLogEnv), os.Getenv(fakeRuntimeLocalBaseEnv) == "1", os.Getenv(fakeRuntimeStateEnv), os.Getenv(fakeRuntimeErrorEnv)))
	}

	dir, err := os.MkdirTemp("", "md-git-config-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, entry := range [][2]string{
		{"GIT_CONFIG_COUNT", "0"},
		{"GIT_CONFIG_GLOBAL", config},
		{"GIT_CONFIG_NOSYSTEM", "1"},
	} {
		if err := os.Setenv(entry[0], entry[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	exitCode := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func isFakeGitExecutable(name string) bool {
	base := filepath.Base(name)
	return base == "git" || base == "git.exe"
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

func TestAgentContainerPaths(t *testing.T) {
	t.Parallel()
	paths := agentContainerPaths()
	for _, want := range []string{"/home/user/src", "/home/user/.agents"} {
		if !slices.Contains(paths, want) {
			t.Errorf("agentContainerPaths() = %v, want %s", paths, want)
		}
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
		logger := slog.New(slog.NewTextHandler(io.MultiWriter(&buf, testLogWriter{t: t}), testLoggerOptions()))
		c := &Client{Logger: logger, Runtime: testRuntime(t, "docker", logger, nil)}
		c.Logger.Log(t.Context(), slog.LevelDebug, "client")
		ct, err := c.Container()
		if err != nil {
			t.Fatal(err)
		}
		ct.Logger.Log(t.Context(), slog.LevelDebug, "container")

		got := buf.String()
		for _, want := range []string{`msg=client`, `msg=container`, `cntr=md-agent-`} {
			if !strings.Contains(got, want) {
				t.Fatalf("log output = %q, want %q", got, want)
			}
		}
	})
	t.Run("specialized_user_owner", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name        string
			runtimeName string
			rootless    bool
			want        string
		}{
			{name: "docker_rootful", runtimeName: "docker", want: "123:456"},
			{name: "docker_rootless", runtimeName: "docker", rootless: true, want: "123:456"},
			{name: "podman_rootful", runtimeName: "podman", want: "123:456"},
			{name: "podman_rootless", runtimeName: "podman", rootless: true, want: "1000:1000"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := specializedUserOwner(tc.runtimeName, tc.rootless, "123:456"); got != tc.want {
					t.Errorf("specializedUserOwner() = %q, want %q", got, tc.want)
				}
			})
		}
	})
	t.Run("host_user_owner", func(t *testing.T) {
		t.Parallel()
		// A root host passes no MD_HOST_UID, so start.sh leaves the image account
		// at the fixed contract identity instead of chowning by name, which a base
		// image without that account cannot resolve.
		for _, tc := range []struct {
			name     string
			uid, gid int
			want     string
		}{
			{name: "host_identity", uid: 123, gid: 456, want: "123:456"},
			{name: "root", uid: 0, gid: 0, want: "1000:1000"},
			{name: "root_uid", uid: 0, gid: 456, want: "1000:1000"},
			{name: "root_gid", uid: 123, gid: 0, want: "1000:1000"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := hostUserOwnerFor(tc.uid, tc.gid); got != tc.want {
					t.Errorf("hostUserOwnerFor(%d, %d) = %q, want %q", tc.uid, tc.gid, got, tc.want)
				}
			})
		}
	})
	t.Run("AgentMounts", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		logger := testLogger(t)
		c, err := newClient(home, logger, testRuntime(t, "docker", logger, nil), io.Discard)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		mounts, err := c.AgentMounts(
			HarnessMounts[HarnessClaude],
			AgentPaths{HomePaths: []string{"custom-ro"}, ReadOnly: true},
		)
		if err != nil {
			t.Fatalf("AgentMounts: %v", err)
		}
		wantDirs := []string{
			filepath.Join(home, ".agents"),
			filepath.Join(home, ".claude"),
			filepath.Join(home, ".config", "md"),
			filepath.Join(home, "custom-ro"),
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
			return m.HostPath == filepath.Join(home, ".agents") && m.ContainerPath == "/home/user/.agents"
		}) {
			t.Fatalf("AgentMounts missing shared agent skills mount: %+v", mounts)
		}
		if slices.ContainsFunc(mounts, func(m Mount) bool {
			return m.HostPath == filepath.Join(home, ".config", "agents") || m.ContainerPath == "/home/user/.config/agents"
		}) {
			t.Fatalf("AgentMounts contains non-standard agents config mount: %+v", mounts)
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
		if !slices.ContainsFunc(mounts, func(m Mount) bool {
			return m.HostPath == filepath.Join(home, "custom-ro") && m.ContainerPath == "/home/user/custom-ro" && m.ReadOnly
		}) {
			t.Fatalf("AgentMounts missing custom read-only mount: %+v", mounts)
		}
		target, err := os.Readlink(filepath.Join(home, ".claude.json"))
		if err != nil {
			t.Fatalf("Readlink .claude.json: %v", err)
		}
		if target != filepath.Join(home, ".claude", "claude.json") {
			t.Fatalf(".claude.json target = %q, want %q", target, filepath.Join(home, ".claude", "claude.json"))
		}
	})
	t.Run("AgentMountsSymlinkTargets", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		logger := testLogger(t)
		c, err := newClient(home, logger, testRuntime(t, "docker", logger, nil), io.Discard)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		fooSkills := filepath.Join(home, "foo_skills")
		barSkills := filepath.Join(home, "bar_skills")
		if err := os.MkdirAll(barSkills, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("bar_skills", fooSkills); err != nil {
			t.Fatal(err)
		}
		piDir := filepath.Join(home, ".pi")
		if err := os.MkdirAll(piDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../foo_skills", filepath.Join(piDir, "skills")); err != nil {
			t.Fatal(err)
		}
		sharedSkill := filepath.Join(home, "shared_skill")
		if err := os.MkdirAll(sharedSkill, 0o700); err != nil {
			t.Fatal(err)
		}
		agentSkills := filepath.Join(home, ".agents", "skills")
		if err := os.MkdirAll(agentSkills, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../shared_skill", filepath.Join(agentSkills, "shared")); err != nil {
			t.Fatal(err)
		}

		mounts, err := c.AgentMounts(HarnessMounts[HarnessPi])
		if err != nil {
			t.Fatalf("AgentMounts: %v", err)
		}
		resolvedBarSkills, err := filepath.EvalSymlinks(barSkills)
		if err != nil {
			t.Fatal(err)
		}
		resolvedSharedSkill, err := filepath.EvalSymlinks(sharedSkill)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []Mount{
			{HostPath: resolvedBarSkills, ContainerPath: "/home/user/foo_skills"},
			{HostPath: resolvedSharedSkill, ContainerPath: "/home/user/shared_skill"},
		} {
			if !slices.ContainsFunc(mounts, func(m Mount) bool {
				return m.HostPath == want.HostPath && m.ContainerPath == want.ContainerPath
			}) {
				t.Fatalf("AgentMounts missing symlink target mount %+v: %+v", want, mounts)
			}
		}
	})
	t.Run("Container", func(t *testing.T) {
		t.Parallel()
		c := testClient(t)
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
				ct, err := c.Container(Repo{GitRoot: tt.gitRoot, Branches: []string{"main"}})
				if err != nil {
					t.Fatal(err)
				}
				if ct.Repos[0].ContainerPath != tt.wantRepo {
					t.Errorf("ContainerPath = %q, want %q", ct.Repos[0].ContainerPath, tt.wantRepo)
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
			c, err := newClient(tmp, testLogger(t), nil, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.Runtime == nil || c.Runtime.Name() == "" {
				t.Error("New() should set Runtime")
			}
		})
		t.Run("explicit", func(t *testing.T) {
			t.Parallel()
			logger := testLogger(t)
			c := &Client{Logger: logger, Runtime: testRuntime(t, "podman", logger, nil)}
			if c.Runtime.Name() != "podman" {
				t.Errorf("Runtime.Name() = %q, want %q", c.Runtime.Name(), "podman")
			}
		})
	})
}

// TestContainerNotListening checks the failure md reports when a started
// container is not reachable. Docker drops the port bindings of a container that
// exited before md read them, so the container's own log has to carry the reason.
func TestContainerNotListening(t *testing.T) { //nolint:paralleltest // fakeRuntime uses t.Setenv, which cannot run in parallel tests.
	home := t.TempDir()
	c := newTestClient(t, home, fakeRuntime(t, filepath.Join(t.TempDir(), "runtime.log"), true))
	ct, err := c.Container()
	if err != nil {
		t.Fatal(err)
	}
	ct.Name = "md-one"
	ct.State = "exited"
	err = ct.containerNotListening(t.Context(), "has no SSH port mapping")
	if err == nil {
		t.Fatal("containerNotListening: got nil error")
	}
	for _, want := range []string{"has no SSH port mapping", "state exited", "md startup is missing sshd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	// A running container has no exit to explain, so no log is attached.
	ct.State = "running"
	err = ct.containerNotListening(t.Context(), "has no SSH port mapping")
	if err == nil || strings.Contains(err.Error(), "last container log lines") {
		t.Errorf("running container error = %v, want the plain port-mapping error", err)
	}
}

func TestBuildSpecializedImage(t *testing.T) { //nolint:tparallel // fakeRuntime uses t.Setenv, which cannot run in parallel tests.
	t.Run("uses_local_remote_base_when_pull_fails", func(t *testing.T) { //nolint:paralleltest // fakeRuntime uses t.Setenv, which cannot run in parallel tests.
		home := t.TempDir()
		logPath := filepath.Join(home, "runtime.log")
		c := newTestClient(t, home, fakeRuntime(t, logPath, true))
		var stdout strings.Builder
		var stderr strings.Builder

		imageID, err := c.buildSpecializedImage(t.Context(), &stdout, &stderr, "md-specialized-test", "ghcr.io/caic-xyz/md-user:latest", PlatformLinuxAMD64.String(), nil, nil, false)
		if err != nil {
			t.Fatalf("buildSpecializedImage: %v", err)
		}
		if imageID != "sha256:specialized" {
			t.Fatalf("imageID = %q, want sha256:specialized", imageID)
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

		_, err := c.buildSpecializedImage(t.Context(), io.Discard, io.Discard, "md-specialized-test", "ghcr.io/caic-xyz/md-user:latest", PlatformLinuxAMD64.String(), nil, nil, false)
		if err == nil {
			t.Fatal("buildSpecializedImage succeeded, want pull failure")
		}
		if !strings.Contains(err.Error(), "pulling base image") {
			t.Fatalf("err = %v, want pull failure", err)
		}
	})

	t.Run("untags_stale_specialized_image_after_base_pull_changes", func(t *testing.T) {
		t.Parallel()
		const (
			imageName = "md-specialized-test"
			baseImage = "ghcr.io/caic-xyz/md-user:latest"
		)
		c, rt := newImageDecisionTestClient(t, imageName, baseImage)
		rt.baseID = "sha256:old-base"
		rt.baseDigest = "ghcr.io/caic-xyz/md-user@sha256:old-base"
		rt.pulledBaseID = "sha256:new-base"
		rt.pulledBaseDigest = "ghcr.io/caic-xyz/md-user@sha256:new-base"
		rt.remoteManifest = "sha256:new-manifest"

		imageID, err := c.buildSpecializedImage(t.Context(), io.Discard, io.Discard, imageName, baseImage, PlatformLinuxAMD64.String(), nil, nil, true)
		if err != nil {
			t.Fatalf("buildSpecializedImage: %v", err)
		}
		if imageID != "sha256:specialized" {
			t.Fatalf("imageID = %q, want sha256:specialized", imageID)
		}
		if !slices.Equal(rt.untagged, []string{imageName}) {
			t.Fatalf("untagged = %v, want [%s]", rt.untagged, imageName)
		}
		if rt.builds != 1 {
			t.Fatalf("builds = %d, want 1", rt.builds)
		}
	})
}

func TestImageBuildNeeded(t *testing.T) {
	t.Parallel()
	const (
		imageName = "md-specialized-test"
		baseImage = "ghcr.io/caic-xyz/md-user:latest"
	)
	tests := []struct {
		name            string
		localBase       bool
		storedManifest  string
		remoteManifest  string
		wantNeeded      bool
		wantUntagged    bool
		wantRemoteCalls int
	}{
		{
			name:            "remote_manifest_stale_reports_needed_without_untag",
			storedManifest:  "sha256:old",
			remoteManifest:  "sha256:new",
			wantNeeded:      true,
			wantRemoteCalls: 1,
		},
		{
			name:            "local_base_skips_remote_staleness",
			localBase:       true,
			storedManifest:  "sha256:old",
			remoteManifest:  "sha256:new",
			wantRemoteCalls: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, rt := newImageDecisionTestClient(t, imageName, baseImage)
			rt.localBase = tt.localBase
			rt.storedManifest = tt.storedManifest
			rt.remoteManifest = tt.remoteManifest

			got := c.imageBuildNeeded(t.Context(), imageName, baseImage, PlatformLinuxAMD64.String(), nil)
			if got != tt.wantNeeded {
				t.Fatalf("imageBuildNeeded() = %t, want %t", got, tt.wantNeeded)
			}
			if got := len(rt.untagged) > 0; got != tt.wantUntagged {
				t.Fatalf("untagged = %v, want untagged %t", rt.untagged, tt.wantUntagged)
			}
			if rt.remoteManifestCalls != tt.wantRemoteCalls {
				t.Fatalf("remote manifest calls = %d, want %d", rt.remoteManifestCalls, tt.wantRemoteCalls)
			}
		})
	}
}

func TestEnsureImage(t *testing.T) {
	t.Parallel()
	const baseImage = "ghcr.io/caic-xyz/md-user:latest"
	c, rt := newImageDecisionTestClient(t, "", baseImage)
	rt.storedManifest = "sha256:remote"
	rt.remoteManifest = "sha256:remote"
	ct := &Container{Client: c}

	got, err := ct.ensureImage(t.Context(), io.Discard, io.Discard, baseImage, PlatformLinuxAMD64.String(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != rt.imageID {
		t.Fatalf("ensureImage() = %q, want immutable image ID %q", got, rt.imageID)
	}
	if rt.builds != 0 {
		t.Fatalf("builds = %d, want 0", rt.builds)
	}
}

func newImageDecisionTestClient(t *testing.T, imageName, baseImage string) (*Client, *imageDecisionRuntime) {
	home := t.TempDir()
	rt := &imageDecisionRuntime{
		imageName:  imageName,
		baseImage:  baseImage,
		imageID:    "sha256:specialized",
		baseID:     "sha256:base",
		baseDigest: "ghcr.io/caic-xyz/md-user@sha256:base",
		arch:       "amd64",
	}
	c := &Client{
		Home:          home,
		XDGConfigHome: filepath.Join(home, ".config"),
		XDGDataHome:   filepath.Join(home, ".local", "share"),
		XDGStateHome:  filepath.Join(home, ".local", "state"),
		HostKeyPath:   filepath.Join(home, ".config", "md", "ssh_host_ed25519_key"),
		UserKeyPath:   filepath.Join(home, ".ssh", "md"),
		Logger:        testLogger(t),
		Runtime:       rt,
	}
	c.keysDir = filepath.Join(c.XDGConfigHome, "md")
	if err := c.setupSSH(io.Discard); err != nil {
		t.Fatal(err)
	}
	contextSHA, err := keysSHA(c.keysDir, hostUserOwner())
	if err != nil {
		t.Fatal(err)
	}
	rt.contextSHA = contextSHA
	return c, rt
}

type imageDecisionRuntime struct {
	imageName           string
	baseImage           string
	imageID             string
	baseID              string
	baseDigest          string
	contextSHA          string
	storedManifest      string
	remoteManifest      string
	arch                string
	localBase           bool
	pulled              bool
	pulledBaseID        string
	pulledBaseDigest    string
	builds              int
	remoteManifestCalls int
	untagged            []string
}

func (r *imageDecisionRuntime) Name() string {
	return "docker"
}

func (r *imageDecisionRuntime) Executable() string {
	return "docker"
}

func (r *imageDecisionRuntime) Run(_ context.Context, _ string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "pull" {
		r.pulled = true
		return "", nil
	}
	if len(args) > 0 && args[0] == "build" {
		r.builds++
		if err := writeFakeIIDFile(args, r.imageID); err != nil {
			return "", err
		}
		return "", nil
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		image, format, ok := testImageInspectArgs(args)
		if !ok {
			return "", fmt.Errorf("unsupported image inspect args: %s", strings.Join(args, " "))
		}
		if image == r.baseImage {
			return r.inspectBaseImage(format)
		}
		if r.imageName == "" || image == r.imageName {
			return r.inspectSpecializedImage(format)
		}
	}
	return "", fmt.Errorf("unexpected runtime command: %s", strings.Join(args, " "))
}

func (r *imageDecisionRuntime) RunOut(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
	if len(args) > 0 && args[0] == "build" {
		r.builds++
		return writeFakeIIDFile(args, r.imageID)
	}
	return fmt.Errorf("unexpected runtime command: %s", strings.Join(args, " "))
}

func (r *imageDecisionRuntime) List(context.Context) ([]containers.Container, error) {
	return nil, nil
}

func (r *imageDecisionRuntime) InspectContainer(context.Context, string) (*containers.Container, error) {
	return nil, errors.New("not implemented")
}

func (r *imageDecisionRuntime) InspectInfo(context.Context, string) (*containers.InspectInfo, error) {
	return nil, errors.New("not implemented")
}

func (r *imageDecisionRuntime) HostPort(context.Context, string, string) (int32, error) {
	return 0, errors.New("not implemented")
}

func (r *imageDecisionRuntime) ImageArchitecture(context.Context, string) (string, error) {
	return r.arch, nil
}

func (r *imageDecisionRuntime) RemoteManifestDigest(context.Context, string, string) (string, error) {
	r.remoteManifestCalls++
	return r.remoteManifest, nil
}

func (r *imageDecisionRuntime) BaseImageIsLocal(context.Context, string) bool {
	return r.localBase
}

func (r *imageDecisionRuntime) UntagImage(_ context.Context, image string) error {
	r.untagged = append(r.untagged, image)
	return nil
}

func (r *imageDecisionRuntime) Stats(context.Context, string) (*containers.Stats, error) {
	return nil, errors.New("not implemented")
}

func (r *imageDecisionRuntime) DiskUsage(context.Context, string) (int64, error) {
	return 0, errors.New("not implemented")
}

func (r *imageDecisionRuntime) StatsAll(context.Context, []string) (map[string]*containers.Stats, error) {
	return nil, errors.New("not implemented")
}

func (r *imageDecisionRuntime) WatchStats(context.Context, []string) (iter.Seq2[containers.StatsSample, error], error) {
	return nil, errors.New("not implemented")
}

func (r *imageDecisionRuntime) WatchDieEvents(context.Context, string) (iter.Seq2[containers.Event, error], error) {
	return nil, errors.New("not implemented")
}

func (r *imageDecisionRuntime) IsRootless() bool {
	return false
}

func (r *imageDecisionRuntime) inspectSpecializedImage(format string) (string, error) {
	switch format {
	case "{{.Id}}":
		return r.imageID, nil
	case `{{index .Config.Labels "md.base_digest"}}`:
		return r.baseDigest, nil
	case `{{index .Config.Labels "md.context_sha"}}`:
		return r.contextSHA, nil
	case `{{index .Config.Labels "md.base_manifest_digest"}}`:
		return r.storedManifest, nil
	case `{{index .Config.Labels "md.cache_key"}}`:
		return "", nil
	default:
		return "", fmt.Errorf("unexpected specialized image format: %s", format)
	}
}

func (r *imageDecisionRuntime) inspectBaseImage(format string) (string, error) {
	switch format {
	case "{{index .RepoDigests 0}}":
		if r.pulled && r.pulledBaseDigest != "" {
			return r.pulledBaseDigest, nil
		}
		return r.baseDigest, nil
	case "{{.Id}}":
		if r.pulled && r.pulledBaseID != "" {
			return r.pulledBaseID, nil
		}
		return r.baseID, nil
	default:
		return "", fmt.Errorf("unexpected base image format: %s", format)
	}
}

func testImageInspectArgs(args []string) (image, format string, ok bool) {
	if len(args) != 5 {
		return "", "", false
	}
	if args[2] == "--format" {
		return args[4], args[3], true
	}
	if args[3] == "--format" {
		return args[2], args[4], true
	}
	return "", "", false
}

func newTestClient(t *testing.T, home, runtimePath string) *Client {
	c := &Client{
		Home:          home,
		XDGConfigHome: filepath.Join(home, ".config"),
		XDGDataHome:   filepath.Join(home, ".local", "share"),
		XDGStateHome:  filepath.Join(home, ".local", "state"),
		HostKeyPath:   filepath.Join(home, ".config", "md", "ssh_host_ed25519_key"),
		UserKeyPath:   filepath.Join(home, ".ssh", "md"),
		Logger:        testLogger(t),
	}
	c.Runtime = testRuntime(t, runtimePath, c.Logger, nil)
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

func runFakeGit(args []string) int {
	if len(args) != 0 && args[0] == "diff" {
		marker, err := os.OpenFile(os.Getenv(fakeGitMarkerEnv), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // private path comes from the parent test process.
		if err == nil {
			_ = marker.Close()
			if path := os.Getenv(fakeGitVanishPathEnv); path != "" {
				if err := os.Remove(path); err != nil { //nolint:gosec // private path comes from the parent test process.
					_, _ = fmt.Fprintf(os.Stderr, "removing test path: %v\n", err)
					return 1
				}
				_, _ = fmt.Fprintf(os.Stderr, "fatal: stat '%s': No such file or directory\n", filepath.Base(path))
			} else {
				_, _ = fmt.Fprintln(os.Stderr, "fatal: simulated diff failure")
			}
			return 128
		}
		if !errors.Is(err, os.ErrExist) {
			_, _ = fmt.Fprintf(os.Stderr, "creating fake Git marker: %v\n", err)
			return 1
		}
		if path := os.Getenv(fakeGitVanishPathEnv); path != "" {
			check := exec.CommandContext(context.Background(), os.Getenv(fakeGitExecutableEnv), "ls-files", "--error-unmatch", "--", filepath.Base(path)) //nolint:gosec // executable and path come from the parent test process.
			check.Stdout = io.Discard
			check.Stderr = io.Discard
			if check.Run() == nil {
				_, _ = fmt.Fprintln(os.Stderr, "fatal: vanished path remains in the temporary index")
				return 128
			}
		}
	}
	cmd := exec.CommandContext(context.Background(), os.Getenv(fakeGitExecutableEnv), args...) //nolint:gosec // executable and arguments come from the parent test process.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
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
	if match := os.Getenv(fakeSSHFailureMatchEnv); match != "" && strings.Contains(strings.Join(args[hostIndex+1:], " "), match) {
		_, _ = fmt.Fprintln(os.Stderr, os.Getenv(fakeSSHFailureTextEnv))
		status := 255
		if value := os.Getenv(fakeSSHFailureStatusEnv); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "invalid fake SSH failure status %q: %v\n", value, err)
				return 1
			}
			status = parsed
		}
		return status
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", strings.Join(args[hostIndex+1:], " ")) //nolint:gosec // test fake executes trusted test commands.
	cmd.Env = append(os.Environ(), "LANG=C")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if ok {
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(os.Stderr, "fake ssh: %v\n", err)
		return 1
	}
	return 0
}

func runFakeRuntime(args []string, logPath string, localBase bool, containerState, runtimeError string) int {
	if err := appendFakeCommandLog(logPath, args); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "writing fake runtime log: %v\n", err)
		return 1
	}
	if len(args) >= 2 && args[0] == "inspect" {
		if runtimeError != "" {
			_, _ = fmt.Fprintln(os.Stderr, runtimeError)
			return 1
		}
		return fakeRuntimeContainerInspect(args, containerState)
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return fakeRuntimeInspect(args, localBase)
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "untag" {
		return 0
	}
	if len(args) >= 1 && args[0] == "pull" {
		_, _ = fmt.Fprintln(os.Stderr, "network unreachable")
		return 1
	}
	if len(args) >= 1 && args[0] == "build" {
		if err := writeFakeIIDFile(args, "sha256:specialized"); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "writing fake iidfile: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) >= 1 && args[0] == "rmi" {
		return 0
	}
	if len(args) >= 1 && args[0] == "rm" {
		return 0
	}
	if len(args) >= 1 && args[0] == "exec" && containerState == "exited" {
		_, _ = fmt.Fprintln(os.Stderr, "container is not running")
		return 1
	}
	if len(args) == 3 && args[0] == "cp" && strings.HasSuffix(args[1], ":"+filepath.ToSlash(filepath.Dir(tailscaleDeviceIDPath))+"/.") {
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
	if len(args) >= 1 && args[0] == "logs" {
		_, _ = fmt.Fprintln(os.Stdout, "[start.sh] ERROR: md startup is missing sshd, requested by md start")
		return 0
	}
	if len(args) >= 1 && args[0] == "builder" {
		return 0
	}
	_, _ = fmt.Fprintf(os.Stderr, "unexpected command: %s\n", strings.Join(args, " "))
	return 1
}

func writeFakeIIDFile(args []string, imageID string) error {
	for i, arg := range args {
		if arg == "--iidfile" && i+1 < len(args) {
			return os.WriteFile(args[i+1], []byte(imageID+"\n"), 0o600) //nolint:gosec // test path comes from private temp dir.
		}
		if path, ok := strings.CutPrefix(arg, "--iidfile="); ok {
			return os.WriteFile(path, []byte(imageID+"\n"), 0o600) //nolint:gosec // test path comes from private temp dir.
		}
	}
	return nil
}

func fakeRuntimeContainerInspect(args []string, state string) int {
	if raw, ok := strings.CutPrefix(state, "raw:"); ok {
		_, _ = fmt.Fprintln(os.Stdout, raw)
		return 0
	}
	if state == "" {
		state = "running"
	}
	if len(args) == 4 && args[1] == "md-test" && args[2] == "--format" && args[3] == "{{.Os}}/{{.Architecture}}" {
		_, _ = fmt.Fprintln(os.Stdout, "linux/amd64")
		return 0
	}
	if len(args) == 2 {
		_, _ = fmt.Fprintf(os.Stdout, `[{"Name":%q,"Id":"ctr","Image":"sha256:image","Platform":"linux","Config":{"Image":"base:latest","Labels":{}},"State":{"Status":%q}}]`+"\n", "/"+args[1], state)
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
	if strings.Contains(argLine, "md-specialized-test") && strings.Contains(argLine, "{{.Id}}") {
		_, _ = fmt.Fprintln(os.Stdout, "sha256:specialized")
		return 0
	}
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
			logger := testLogger(t)
			c := &Client{Logger: logger, Runtime: testRuntime(t, "true", logger, nil)}
			if !c.Runtime.BaseImageIsLocal(t.Context(), image) {
				t.Errorf("BaseImageIsLocal(%q) = false, want true", image)
			}
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		logger := testLogger(t)
		c := &Client{Logger: logger, Runtime: testRuntime(t, "false", logger, nil)}
		for _, image := range []string{"ubuntu:latest", "md-user-local:latest", "myteam/image:latest"} {
			if c.Runtime.BaseImageIsLocal(t.Context(), image) {
				t.Errorf("BaseImageIsLocal(%q) = true, want false", image)
			}
		}
		c = &Client{Logger: logger, Runtime: testRuntime(t, "true", logger, nil)}
		for _, image := range []string{"docker.io/library/ubuntu:latest", "ghcr.io/caic-xyz/md-user:latest", "localhost:5000/md-user:latest"} {
			if c.Runtime.BaseImageIsLocal(t.Context(), image) {
				t.Errorf("BaseImageIsLocal(%q) = true, want false", image)
			}
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
		if _, err := rscFS.ReadFile("rsc/specialized/root/start.sh"); err != nil {
			t.Fatalf("embedded rsc/specialized/root/start.sh not found: %v", err)
		}
	})
}

//

var testLogStart = time.Now()

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

type testLogWriter struct {
	t testing.TB
}

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}
