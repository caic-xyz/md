// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for container types and lifecycle.

package md

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/md/git"
)

func runTestGit(t *testing.T, ctx context.Context, wd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from test code
	cmd.Dir = wd
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"LANG=C",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func sameTestPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func writeTestFile(t *testing.T, name, content string) {
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil { //nolint:gosec // test data, world-readable is fine
		t.Fatal(err)
	}
}

func writeTestSSHConfig(t *testing.T, home string) {
	sshConfigDir := filepath.Join(home, ".ssh", "config.d")
	if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshConfigDir, "md-test.conf"), []byte("Host md-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setupPullTest(t *testing.T) (ct *Container, hostDir, containerDir string) {
	ctx := t.Context()
	fakeSSH(t)
	home := t.TempDir()
	writeTestSSHConfig(t, home)

	originDir := filepath.Join(t.TempDir(), "origin.git")
	hostDir = t.TempDir()
	containerDir = t.TempDir()
	runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
	runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
	runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
	runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(hostDir, "shared.txt"), "base\n")
	runTestGit(t, ctx, hostDir, "add", ".")
	runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
	runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
	runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
	runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
	runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
	runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v1\n")
	runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container v1")
	runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

	logger := testLogger(t)
	ct = &Container{
		Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
		Logger: logger,
		Name:   "md-test",
		Repos: []Repo{{
			GitRoot:       hostDir,
			Branches:      []string{"main"},
			ContainerPath: filepath.ToSlash(containerDir),
			DefaultRemote: "origin",
			DefaultBranch: "main",
		}},
	}
	if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
		t.Fatalf("initial Pull: %v", err)
	}
	return ct, hostDir, containerDir
}

func setupMultiBranchPullTest(t *testing.T) (ct *Container, hostDir, containerDir string) {
	ctx := t.Context()
	fakeSSH(t)
	home := t.TempDir()
	writeTestSSHConfig(t, home)

	originDir := filepath.Join(t.TempDir(), "origin.git")
	hostDir = t.TempDir()
	containerDir = t.TempDir()
	runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
	runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
	runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
	runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(hostDir, "main.txt"), "main base\n")
	runTestGit(t, ctx, hostDir, "add", ".")
	runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main base")
	runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
	runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
	runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "feature")
	writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "feature base\n")
	runTestGit(t, ctx, hostDir, "add", ".")
	runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "feature base")
	runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "feature")
	runTestGit(t, ctx, hostDir, "switch", "-q", "main")

	runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
	runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
	runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
	runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

	logger := testLogger(t)
	ct = &Container{
		Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
		Logger: logger,
		Name:   "md-test",
		Repos: []Repo{{
			GitRoot:       hostDir,
			Branches:      []string{"main", "feature"},
			ContainerPath: filepath.ToSlash(containerDir),
			DefaultRemote: "origin",
			DefaultBranch: "main",
		}},
	}
	return ct, hostDir, containerDir
}

// installRejectingHooks installs pre-commit and commit-msg hooks that always
// fail, so a container commit succeeds only when Git skips them.
func installRejectingHooks(t *testing.T, repoDir string) {
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "commit-msg"} {
		path := filepath.Join(hooksDir, name)
		writeTestFile(t, path, "#!/bin/sh\necho rejected by "+name+" >&2\nexit 1\n")
		// #nosec G302 -- Git requires hooks to be executable.
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "''"},
		{"simple", "simple", "simple"},
		{"spaces", "hello world", "'hello world'"},
		{"single_quote", "it's", `'it'\''s'`},
		{"multiple_quotes", "a'b'c", `'a'\''b'\''c'`},
		{"safe_path", "safe-path/to_file.txt", "safe-path/to_file.txt"},
		{"with_spaces", "with spaces", "'with spaces'"},
		{"semicolon", "with;semi", "'with;semi'"},
		{"dollar_cmd", "$(cmd)", "'$(cmd)'"},
		{"backslash", `back\slash`, `'back\slash'`},
		{"newline", "hello\nworld", "'hello\nworld'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shellQuote(tt.in); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShellQuoteArgs(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got := shellQuoteArgs([]string{"printf", "%s\n", "hello world", "$(not-run)", "it's"})
		want := `printf '%s
' 'hello world' '$(not-run)' 'it'\''s'`
		if got != want {
			t.Errorf("shellQuoteArgs() = %q, want %q", got, want)
		}
	})
}

func TestRenderExtraEnv(t *testing.T) {
	t.Parallel()
	got, err := renderExtraEnv([]string{"MULTI=line 1\nline 2", "QUOTE=a'b", "GONE="})
	if err != nil {
		t.Fatal(err)
	}
	want := "MULTI='line 1\nline 2'\nQUOTE='a'\\''b'\nunset GONE\n"
	if string(got) != want {
		t.Errorf("renderExtraEnv() = %q, want %q", got, want)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", string(got)+`printf '%s' "$MULTI"`) //nolint:gosec // script is generated from test literals.
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "line 1\nline 2" {
		t.Errorf("sourced MULTI = %q", out)
	}
}

func TestCheckRepoOverlap(t *testing.T) {
	t.Parallel()
	name := "md-genai-caic-5"
	repos := []Repo{{GitRoot: "/home/user/src/genai", Branches: []string{"caic-5"}}}
	t.Run("no existing containers", func(t *testing.T) {
		t.Parallel()
		if err := checkRepoOverlap(name, repos, nil); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("distinct repo", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name:  "md-caic-caic-2",
			Repos: []Repo{{GitRoot: "/home/user/src/caic", Branches: []string{"caic-2"}}},
		}}
		if err := checkRepoOverlap(name, repos, existing); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("distinct branch, same repo", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name:  "md-genai-main",
			Repos: []Repo{{GitRoot: "/home/user/src/genai", Branches: []string{"main"}}},
		}}
		if err := checkRepoOverlap(name, repos, existing); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("same name is not a conflict with itself", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name:  name,
			Repos: []Repo{{GitRoot: "/home/user/src/genai", Branches: []string{"caic-5"}}},
		}}
		if err := checkRepoOverlap(name, repos, existing); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("overlapping repo and branch as an extra repo in another container", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name: "md-caic-caic-2",
			Repos: []Repo{
				{GitRoot: "/home/user/src/caic", Branches: []string{"caic-2"}},
				{GitRoot: "/home/user/src/genai", Branches: []string{"caic-5"}},
			},
		}}
		err := checkRepoOverlap(name, repos, existing)
		if err == nil {
			t.Fatal("checkRepoOverlap() = nil, want error")
		}
		if !strings.Contains(err.Error(), "md-caic-caic-2") {
			t.Fatalf("checkRepoOverlap() = %v, want mention of md-caic-caic-2", err)
		}
	})
}

// setupDiffTestRepo creates a repository on branch main with one commit,
// tracking host/main as the container's mapped branches do.
func setupDiffTestRepo(t *testing.T, ctx context.Context) string {
	dir := t.TempDir()
	runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
	runTestGit(t, ctx, dir, "config", "user.name", "Test")
	runTestGit(t, ctx, dir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(dir, "base.txt"), "base\n")
	runTestGit(t, ctx, dir, "add", ".")
	runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
	runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
	runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
	runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
	runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")
	return dir
}

// runTestDiffCommand runs the generated container diff command and returns its
// trimmed stdout and its stderr.
func runTestDiffCommand(t *testing.T, ctx context.Context, req *diffRequest) (stdout, stderr string) {
	cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(req)) //nolint:gosec // repo path is a test temp dir
	cmd.Env = append(os.Environ(), "LANG=C")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	return strings.TrimSpace(out.String()), errOut.String()
}

func fakeGitDiffOnce(t *testing.T, vanishPath string) []string {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	t.Cleanup(func() {
		if err := removeAllWithRetry(binDir); err != nil {
			t.Errorf("removing fake Git dir %q: %v", binDir, err)
		}
	})
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := linkOrCopyExecutable(executable, filepath.Join(binDir, name)); err != nil {
		t.Fatal(err)
	}
	return []string{
		fakeGitDiffFailureEnv + "=1",
		fakeGitExecutableEnv + "=" + realGit,
		fakeGitMarkerEnv + "=" + filepath.Join(t.TempDir(), "failed"),
		fakeGitVanishPathEnv + "=" + vanishPath,
		"LANG=C",
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

// gitShimPATH writes a git wrapper that runs inject before delegating to the
// real git, and returns a PATH value that puts the wrapper first.
func gitShimPATH(t *testing.T, inject string) string {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + inject + "\nexec " + shellQuote(gitPath) + ` "$@"` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // the wrapper must be executable.
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestDiff(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "old\n")
		writeTestFile(t, filepath.Join(dir, "staged.txt"), "old\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "init")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "new\n")
		writeTestFile(t, filepath.Join(dir, "staged.txt"), "new\n")
		runTestGit(t, ctx, dir, "add", "staged.txt")
		writeTestFile(t, filepath.Join(dir, "untracked.txt"), "new\n")

		diffCommand := gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main"})
		if strings.Count(diffCommand, "git diff ") != 1 {
			t.Fatalf("diff command runs multiple git diff invocations: %s", diffCommand)
		}
		if strings.Contains(diffCommand, "git diff --no-index") {
			t.Fatalf("diff command uses separate no-index diff invocation: %s", diffCommand)
		}

		cmd := exec.CommandContext(ctx, "bash", "-c", diffCommand) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		out := stdout.String()
		for _, name := range []string{"tracked.txt", "staged.txt", "untracked.txt"} {
			if !strings.Contains(out, name) {
				t.Errorf("diff output missing %q:\n%s", name, out)
			}
		}
		if got := runTestGit(t, ctx, dir, "diff", "--cached", "--name-only"); got != "staged.txt" {
			t.Errorf("cached diff = %q, want staged.txt", got)
		}
		status := runTestGit(t, ctx, dir, "status", "--short")
		for _, want := range []string{" M tracked.txt", "M  staged.txt", "?? untracked.txt"} {
			if !strings.Contains(status, want) {
				t.Errorf("status missing %q:\n%s", want, status)
			}
		}
	})
	t.Run("valid_retries_when_a_file_vanishes", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		vanishedPath := filepath.Join(dir, "vanished.txt")
		writeTestFile(t, vanishedPath, "vanished\n")
		writeTestFile(t, filepath.Join(dir, "retained.txt"), "retained\n")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), fakeGitDiffOnce(t, vanishedPath)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "retained.txt") {
			t.Errorf("diff --stat output missing retained.txt:\n%s", stdout.String())
		}
		if strings.Contains(stdout.String(), "vanished.txt") {
			t.Errorf("diff --stat output contains vanished.txt:\n%s", stdout.String())
		}
		if strings.Contains(stderr.String(), "fatal: stat") {
			t.Errorf("diff leaked the recovered transient error:\n%s", stderr.String())
		}
	})
	t.Run("valid_skips_an_untracked_file_that_vanishes_before_add", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		vanishedPath := filepath.Join(dir, "vanished.txt")
		writeTestFile(t, vanishedPath, "vanished\n")
		writeTestFile(t, filepath.Join(dir, "retained.txt"), "retained\n")

		shim := gitShimPATH(t, `if [ "$1" = add ] && [ ! -e "$MD_TEST_ADD_MARKER" ]; then
	: > "$MD_TEST_ADD_MARKER"
	rm -f `+shellQuote(vanishedPath)+`
fi`)
		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C", "PATH="+shim, "MD_TEST_ADD_MARKER="+filepath.Join(t.TempDir(), "marker"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "retained.txt") {
			t.Errorf("diff --stat output missing retained.txt:\n%s", stdout.String())
		}
		if strings.Contains(stdout.String(), "vanished.txt") || strings.Contains(stderr.String(), "did not match any files") {
			t.Errorf("skipped add failure leaked:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
	})
	t.Run("valid_ignores_an_untracked_nested_repository", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "retained.txt"), "retained\n")
		nested := filepath.Join(dir, "nested")
		if err := os.Mkdir(nested, 0o750); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, ctx, nested, "init", "-q", "--initial-branch=main")

		stdout, stderr := runTestDiffCommand(t, ctx, &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})
		if !strings.Contains(stdout, "retained.txt") {
			t.Errorf("diff --stat output missing retained.txt:\n%s", stdout)
		}
		if strings.Contains(stderr, "adding files failed") || strings.Contains(stdout, "nested/") {
			t.Errorf("nested repository leaked into the diff:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})
	t.Run("error_does_not_retry_unrelated_git_failure", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "untracked.txt"), "untracked\n")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), fakeGitDiffOnce(t, "")...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("diff command unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "fatal: simulated diff failure") {
			t.Errorf("diff did not surface the original Git failure:\n%s", stderr.String())
		}
	})
	t.Run("valid_uses_merge_base_when_upstream_moves", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "base.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		baseCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")

		writeTestFile(t, filepath.Join(dir, "local.txt"), "local\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "local")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "upstream-update", baseCommit)
		writeTestFile(t, filepath.Join(dir, "upstream.txt"), "upstream\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "upstream")
		upstreamCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", upstreamCommit)
		runTestGit(t, ctx, dir, "checkout", "-q", "main")

		stdout, stderr := runTestDiffCommand(t, ctx, &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}})
		if !strings.Contains(stdout, "local.txt") {
			t.Errorf("diff output missing local.txt:\n%s", stdout)
		}
		if strings.Contains(stdout, "upstream.txt") {
			t.Errorf("diff output includes updated upstream file:\n%s", stdout)
		}
		if !strings.Contains(stderr, `branch "main" has no recorded host integration point`) {
			t.Errorf("stderr missing the integration-point note:\n%s", stderr)
		}
	})
	t.Run("valid_since_sync_point", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "synced.txt"), "synced\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "synced")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", "HEAD")

		writeTestFile(t, filepath.Join(dir, "committed.txt"), "committed\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "after the sync point")
		writeTestFile(t, filepath.Join(dir, "untracked.txt"), "untracked\n")

		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if want := "committed.txt\nuntracked.txt"; stdout != want {
			t.Errorf("diff --name-only = %q, want %q\nstderr:\n%s", stdout, want, stderr)
		}
		req.full = true
		stdout, stderr = runTestDiffCommand(t, ctx, req)
		if want := "committed.txt\nsynced.txt\nuntracked.txt"; stdout != want {
			t.Errorf("full diff --name-only = %q, want %q\nstderr:\n%s", stdout, want, stderr)
		}
	})
	t.Run("valid_since_sync_point_survives_rebase_and_gc", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		baseCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "work")
		syncCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", syncCommit)

		// The upstream gains a commit and the agent rebases onto it, which makes
		// the recorded sync point unreachable from every branch.
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "upstream-update", baseCommit)
		writeTestFile(t, filepath.Join(dir, "upstream.txt"), "upstream\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "upstream")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		runTestGit(t, ctx, dir, "branch", "-q", "-D", "upstream-update")
		runTestGit(t, ctx, dir, "rebase", "-q", "host/main")
		runTestGit(t, ctx, dir, "update-ref", "-d", "ORIG_HEAD")
		runTestGit(t, ctx, dir, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all")
		runTestGit(t, ctx, dir, "gc", "-q", "--prune=now")

		// Only the sync ref keeps the base of the diff alive.
		runTestGit(t, ctx, dir, "cat-file", "-e", syncCommit)
		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		// The rebase replayed work.txt unchanged, so the diff from the sync point
		// holds the upstream commit only.
		if stdout != "upstream.txt" {
			t.Errorf("diff --name-only = %q, want upstream.txt", stdout)
		}
		if !strings.Contains(stderr, "moved onto a newer upstream") {
			t.Errorf("stderr missing the rebase note:\n%s", stderr)
		}
	})
	t.Run("valid_since_sync_point_during_a_rebase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")
		writeTestFile(t, filepath.Join(dir, "synced.txt"), "synced\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "work")
		// The sync point is the work commit, which already holds synced.txt. A
		// diff from it therefore reports only the conflicted file, while the
		// upstream merge base would also report synced.txt as added.
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", "HEAD")

		// Conflict with the rebase target so the rebase stops with HEAD detached.
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "rebase-target", "HEAD~1")
		writeTestFile(t, filepath.Join(dir, "work.txt"), "target\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "target")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		rebase := exec.CommandContext(ctx, "git", "rebase", "rebase-target")
		rebase.Dir = dir
		rebase.Env = append(os.Environ(), "LANG=C")
		if out, err := rebase.CombinedOutput(); err == nil {
			t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
		}
		if got := runTestGit(t, ctx, dir, "branch", "--show-current"); got != "" {
			t.Fatalf("branch --show-current = %q, want empty during the rebase", got)
		}

		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if stdout != "work.txt" {
			t.Errorf("diff --name-only = %q, want work.txt only, so the sync point was the base\nstderr:\n%s", stdout, stderr)
		}
		if strings.Contains(stderr, "no recorded host integration point") {
			t.Errorf("stderr claims no integration point while a rebase is in progress:\n%s", stderr)
		}
	})
	t.Run("valid_reset_behind_the_sync_point_omits_the_upstream_note", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		baseCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		writeTestFile(t, filepath.Join(dir, "upstream.txt"), "upstream\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "upstream")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", "HEAD")
		// The agent drops the upstream commit instead of moving onto a new one.
		runTestGit(t, ctx, dir, "reset", "--hard", "-q", baseCommit)

		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if stdout != "upstream.txt" {
			t.Errorf("diff --name-only = %q, want upstream.txt\nstderr:\n%s", stdout, stderr)
		}
		if strings.Contains(stderr, "moved onto a newer upstream") {
			t.Errorf("stderr claims the branch moved onto a newer upstream:\n%s", stderr)
		}
	})
	t.Run("valid_quiet_omits_base_notes", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")

		stdout, stderr := runTestDiffCommand(t, ctx, &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--quiet"}, quiet: true})
		if stdout != "" || stderr != "" {
			t.Errorf("quiet diff = stdout %q, stderr %q; want both empty", stdout, stderr)
		}
	})
	t.Run("valid_since_sync_point_without_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "base.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"caic-1", "HEAD")
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")

		req := &diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if stdout != "work.txt" {
			t.Errorf("diff --name-only = %q, want work.txt\nstderr:\n%s", stdout, stderr)
		}
	})
	t.Run("valid_legacy_base_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "old\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "init")
		runTestGit(t, ctx, dir, "branch", "base")
		runTestGit(t, ctx, dir, "checkout", "-q", "-B", "caic-2", "base")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=base", "caic-2")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "new\n")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-2", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if out := stdout.String(); !strings.Contains(out, "tracked.txt") {
			t.Errorf("diff output missing tracked.txt:\n%s", out)
		}
	})
	t.Run("valid_rebase_in_progress", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")

		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "rebase-target")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "target\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "target")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "local")

		rebase := exec.CommandContext(ctx, "git", "rebase", "rebase-target")
		rebase.Dir = dir
		rebase.Env = append(os.Environ(), "LANG=C")
		if out, err := rebase.CombinedOutput(); err == nil {
			t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git", "rebase-merge", "head-name")); err != nil {
			t.Fatalf("rebase metadata missing: %v", err)
		}

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != "tracked.txt" {
			t.Errorf("diff --name-only = %q, want tracked.txt", got)
		}
	})
	t.Run("error_checked_out_branch_has_no_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.origin.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=origin/main", "caic-1")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "fix-$(touch-pwn)")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("diff command unexpectedly succeeded")
		}
		for _, want := range []string{
			`current branch "fix-$(touch-pwn)" has no upstream branch configured`,
			`The container's primary branch is "caic-1".`,
			"git switch caic-1",
			"git branch --set-upstream-to=caic-1",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("error missing %q:\n%s", want, stderr.String())
			}
		}
		if strings.Contains(stderr.String(), "git branch --set-upstream-to=caic-1 fix-$(touch-pwn)") {
			t.Fatalf("error suggests an unsafe command:\n%s", stderr.String())
		}
	})
	t.Run("error_primary_branch_has_no_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("diff command unexpectedly succeeded")
		}
		for _, want := range []string{
			`primary branch "caic-1" has no upstream branch configured`,
			"git branch --set-upstream-to=origin/main caic-1",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("error missing %q:\n%s", want, stderr.String())
			}
		}
	})
	t.Run("error_detached_head_suggests_primary_branch", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")
		runTestGit(t, ctx, dir, "switch", "-q", "--detach")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("diff command unexpectedly succeeded")
		}
		for _, want := range []string{
			"HEAD is detached",
			"git switch caic-1",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("error missing %q:\n%s", want, stderr.String())
			}
		}
	})
}

func TestPlanFork(t *testing.T) {
	t.Parallel()
	source := []Repo{
		{GitRoot: "/src/a", Branches: []string{"main", "feat"}},
		{GitRoot: "/src/b", Branches: []string{"main"}},
	}

	t.Run("orders_source_then_extras", func(t *testing.T) {
		t.Parallel()
		// Extras keep caller order; source dests follow source order regardless
		// of where they appear in the spec.
		ctx := t.Context()
		xDir := t.TempDir()
		xRemote := filepath.Join(t.TempDir(), "x.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=dev", xRemote)
		runTestGit(t, ctx, xDir, "init", "-q", "--initial-branch=dev")
		runTestGit(t, ctx, xDir, "commit", "-q", "--allow-empty", "-m", "dev")
		runTestGit(t, ctx, xDir, "remote", "add", "origin", xRemote)
		runTestGit(t, ctx, xDir, "remote", "add", "md-other", "user@md-other:/home/user/src/"+filepath.Base(xDir))
		runTestGit(t, ctx, xDir, "push", "-q", "-u", "origin", "dev")
		runTestGit(t, ctx, xDir, "config", "branch.dev.pushRemote", "md-other")
		runTestGit(t, ctx, xDir, "remote", "set-head", "origin", "dev")
		yDir := t.TempDir()
		yRemote := filepath.Join(t.TempDir(), "y.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", yRemote)
		runTestGit(t, ctx, yDir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, yDir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, yDir, "branch", "topic")
		runTestGit(t, ctx, yDir, "remote", "add", "origin", yRemote)
		runTestGit(t, ctx, yDir, "push", "-q", "-u", "origin", "main", "topic")
		spec := []ForkRepo{
			{GitRoot: xDir, SourceBranches: []string{"dev"}, TagRegexp: "^v", DestPrimary: "x0"},
			{GitRoot: "/src/b", SourceBranches: []string{"main"}, DestPrimary: "b0"},
			{GitRoot: "/src/a", DestPrimary: "a0"}, // SourceBranches omitted: allowed for a source repo.
			{GitRoot: yDir, SourceBranches: []string{"main", "topic"}, ContainerPath: "/home/user/src/nested/y", DestPrimary: "y0"},
		}
		plan, err := planFork(ctx, testLogger(t), "", source, spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.extraRepos[1].ContainerPath; got != "/home/user/src/nested/y" {
			t.Errorf("extra /src/y ContainerPath = %q, want /home/user/src/nested/y", got)
		}
		if want := []string{"a0", "b0"}; !slices.Equal(plan.srcDest, want) {
			t.Errorf("srcDest = %v, want %v", plan.srcDest, want)
		}
		if want := []string{"x0", "y0"}; !slices.Equal(plan.extraDest, want) {
			t.Errorf("extraDest = %v, want %v", plan.extraDest, want)
		}
		if len(plan.extraRepos) != 2 || plan.extraRepos[0].GitRoot != xDir || plan.extraRepos[1].GitRoot != yDir {
			t.Fatalf("extraRepos = %+v, want x then y", plan.extraRepos)
		}
		if plan.extraRepos[0].TagRegexp != "^v" {
			t.Errorf("extra x TagRegexp = %q, want ^v", plan.extraRepos[0].TagRegexp)
		}
		if want := []string{"origin"}; !slices.Equal(plan.extraRepos[0].Remotes, want) {
			t.Errorf("extra x remotes = %v, want %v", plan.extraRepos[0].Remotes, want)
		}
		base, err := plan.extraRepos[0].resolveForkExtraBranchBase(ctx, testLogger(t), "dev", "x0")
		if err != nil {
			t.Fatal(err)
		}
		if base.pushRemote != "" {
			t.Errorf("extra x push remote = %q, want empty", base.pushRemote)
		}
		if want := []string{"main", "topic"}; !slices.Equal(plan.extraRepos[1].Branches, want) {
			t.Errorf("extra /src/y branches = %v, want %v", plan.extraRepos[1].Branches, want)
		}
	})

	errCases := []struct {
		name string
		spec []ForkRepo
	}{
		{"missing_source_repo", []ForkRepo{{GitRoot: "/src/a", SourceBranches: []string{"main", "feat"}, DestPrimary: "a0"}}}, // /src/b missing.
		{"source_branch_mismatch", []ForkRepo{
			{GitRoot: "/src/a", SourceBranches: []string{"other"}, DestPrimary: "a0"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
		{"empty_dest", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: ""},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
		{"duplicate_gitroot", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "a0"},
			{GitRoot: "/src/a", DestPrimary: "a1"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
		// Reusing a mapped branch would map one host branch into two containers,
		// and Fork force-resets the destination host branch.
		{"dest_reuses_source_primary", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "a0"},
			{GitRoot: "/src/b", DestPrimary: "main"},
		}},
		{"dest_reuses_source_non_primary_branch", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "feat"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := planFork(t.Context(), testLogger(t), "", source, tc.spec); err == nil {
				t.Fatal("planFork error = nil, want error")
			}
		})
	}

	// An added repo resolves its own branches, so its destination is checked
	// against those rather than against a source container's mapping.
	t.Run("error_added_repo_dest_reuses_its_own_branch", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		remote := filepath.Join(t.TempDir(), "z.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", remote)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, dir, "branch", "topic")
		runTestGit(t, ctx, dir, "remote", "add", "origin", remote)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main", "topic")
		spec := []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "a0"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
			{GitRoot: dir, SourceBranches: []string{"main", "topic"}, DestPrimary: "topic"},
		}
		if _, err := planFork(ctx, testLogger(t), "", source, spec); err == nil {
			t.Fatal("planFork error = nil, want error")
		}
	})
}

func TestContainer(t *testing.T) { //nolint:tparallel // Pull uses fakeSSH with t.Setenv.
	t.Run("purge_stopped_tailscale_container_before_authentication", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + logPath,
			fakeRuntimeStateEnv + "=exited",
		}
		logger := testLogger(t)
		ct := &Container{
			Client:    &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger:    logger,
			Name:      "md-test",
			State:     "exited",
			Tailscale: true,
		}
		var stdout bytes.Buffer
		if err := ct.Purge(t.Context(), &stdout, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := stdout.String(); got != "Removed md-test\n" {
			t.Errorf("Purge output = %q, want %q", got, "Removed md-test\n")
		}
		logData, err := os.ReadFile(logPath) //nolint:gosec // private test log.
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(logData), "rm -f -v md-test") {
			t.Errorf("runtime log missing container removal:\n%s", logData)
		}
	})
	t.Run("configure_global_git_identity_uses_host_global_config", func(t *testing.T) {
		ctx := t.Context()
		fakeSSH(t)
		sshLogPath := filepath.Join(t.TempDir(), "ssh.log")
		t.Setenv(fakeSSHLogEnv, sshLogPath)
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		config := filepath.Join(t.TempDir(), "gitconfig")
		writeTestFile(t, config, "[user]\n\tname = Global User\n\temail = global@example.com\n")

		logger := testLogger(t)
		ct := &Container{
			Client: &Client{
				Home:          home,
				XDGConfigHome: filepath.Join(home, ".config"),
				Logger:        logger,
				Runtime:       testRunningRuntime(t, logger),
				env:           []string{"GIT_CONFIG_GLOBAL=" + config, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=0"},
			},
			Logger: logger,
			Name:   "md-test",
		}
		if err := ct.configureGlobalGitIdentity(ctx, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		sshLog, err := os.ReadFile(sshLogPath) //nolint:gosec // test log is under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"git config --global --replace-all user.name 'Global User'",
			"git config --global --replace-all user.email global@example.com",
		} {
			if !strings.Contains(string(sshLog), want) {
				t.Errorf("global identity command = %q, want %q", sshLog, want)
			}
		}
	})
	t.Run("configure_container_remotes_propagates_git_identity", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		fakeSSH(t)
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		hostDir := t.TempDir()
		containerDir := t.TempDir()
		runTestGit(t, ctx, hostDir, "init", "-q")
		runTestGit(t, ctx, hostDir, "config", "user.name", "Marc-Antoine Ruel")
		runTestGit(t, ctx, hostDir, "config", "user.email", "maruel@example.com")
		runTestGit(t, ctx, hostDir, "config", "core.hooksPath", "scripts/hooks")
		runTestGit(t, ctx, containerDir, "init", "-q")

		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       hostDir,
				ContainerPath: filepath.ToSlash(containerDir),
			}},
		}
		if err := ct.configureContainerRemotes(ctx, io.Discard, io.Discard, 0, true); err != nil {
			t.Fatal(err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--local", "user.name"); got != "Marc-Antoine Ruel" {
			t.Errorf("container user.name = %q, want Marc-Antoine Ruel", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--local", "user.email"); got != "maruel@example.com" {
			t.Errorf("container user.email = %q, want maruel@example.com", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--local", "core.hooksPath"); got != "scripts/hooks" {
			t.Errorf("container core.hooksPath = %q, want scripts/hooks", got)
		}
	})
	t.Run("launch_rejects_extra_branch_tracking_primary_before_runtime_mutation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		hostDir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
		runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, hostDir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
		runTestGit(t, ctx, hostDir, "remote", "set-head", "origin", "main")
		runTestGit(t, ctx, hostDir, "branch", "feature")
		runTestGit(t, ctx, hostDir, "config", "branch.feature.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.feature.merge", "refs/heads/main")

		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{fakeRuntimeEnv + "=1", fakeRuntimeLogEnv + "=" + logPath}
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Logger: logger, Runtime: testRuntime(t, exe, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:  hostDir,
				Branches: []string{"main", "feature"},
			}},
		}
		err = ct.Launch(ctx, io.Discard, io.Discard, &StartOpts{Quiet: true})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) {
			t.Fatalf("Launch error = %v, want local primary-upstream error", err)
		}
		logData, readErr := os.ReadFile(logPath) //nolint:gosec // private test log.
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if len(logData) != 0 {
			t.Fatalf("Launch invoked runtime before branch validation:\n%s", logData)
		}
	})
	t.Run("operations_reject_extra_branch_tracking_primary", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, _ := setupMultiBranchPullTest(t)
		runTestGit(t, ctx, hostDir, "config", "branch.feature.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.feature.merge", "refs/heads/main")

		for _, op := range []struct {
			name string
			run  func() error
		}{
			{name: "Diff", run: func() error { return ct.Diff(ctx, io.Discard, io.Discard, 0, nil) }},
			{name: "Pull", run: func() error { return ct.Pull(ctx, io.Discard, io.Discard, 0, nil) }},
			{name: "Push", run: func() error {
				_, err := ct.Push(ctx, io.Discard, io.Discard, 0)
				return err
			}},
		} {
			err := op.run()
			if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) {
				t.Errorf("%s error = %v, want local primary-upstream error", op.name, err)
			}
		}
	})
	t.Run("diff_rejects_host_branch_without_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "branch", "--unset-upstream", "main")

		err := ct.Diff(ctx, io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "main" has no upstream`) || !strings.Contains(err.Error(), "git -C "+shellQuote(hostDir)+" branch --set-upstream-to=origin/main main") {
			t.Fatalf("Diff error = %v, want missing host upstream repair command", err)
		}
	})
	t.Run("diff_exit_status_flags_report_differences", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		for _, test := range []struct {
			name       string
			args       []string
			dirty      bool
			wantErr    bool
			wantOutput string
		}{
			{name: "exit_code", args: []string{"--exit-code", "--name-only"}, dirty: true, wantErr: true, wantOutput: "shared.txt"},
			{name: "quiet", args: []string{"--quiet"}, dirty: true, wantErr: true},
			{name: "no_differences", args: []string{"--exit-code"}},
		} {
			t.Run(test.name, func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
				ct, hostDir, containerDir := setupPullTest(t)
				if test.dirty {
					writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "different\n")
				} else {
					runTestGit(t, t.Context(), hostDir, "push", "-q", "origin", "main")
				}
				var stdout, stderr bytes.Buffer
				err := ct.Diff(t.Context(), &stdout, &stderr, 0, &DiffOpts{Args: test.args})
				if got := errors.Is(err, ErrDiffFound); got != test.wantErr {
					t.Fatalf("Diff error is ErrDiffFound = %v, want %v: %v", got, test.wantErr, err)
				} else if !test.wantErr && err != nil {
					t.Fatalf("Diff error = %v, want nil", err)
				}
				if got := strings.TrimSpace(stdout.String()); got != test.wantOutput {
					t.Errorf("Diff output = %q, want %q", got, test.wantOutput)
				}
				if stderr.Len() != 0 {
					t.Errorf("Diff stderr = %q, want empty", stderr.String())
				}
			})
		}
	})
	t.Run("diff_forwards_base_notes_with_exit_status_flags", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		ctx := t.Context()
		ct, _, containerDir := setupPullTest(t)
		runTestGit(t, ctx, containerDir, "update-ref", "-d", containerSyncRefPrefix+"main")
		writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "different\n")

		var stdout, stderr bytes.Buffer
		err := ct.Diff(ctx, &stdout, &stderr, 0, &DiffOpts{Args: []string{"--exit-code", "--name-only"}})
		if !errors.Is(err, ErrDiffFound) {
			t.Fatalf("Diff error = %v, want ErrDiffFound", err)
		}
		if !strings.Contains(stderr.String(), "has no recorded host integration point") {
			t.Errorf("Diff stderr = %q, want the missing sync point note", stderr.String())
		}
	})
	t.Run("host_upstream_repair_command_targets_mapped_repo", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, t.Context(), hostDir, "branch", "--unset-upstream", "main")

		_, _, err := ct.Repos[0].requiredMappedBranchUpstream(t.Context(), ct.Logger, "main")
		if err == nil {
			t.Fatal("requiredMappedBranchUpstream error = nil, want repair guidance")
		}
		_, command, ok := strings.Cut(err.Error(), "run: ")
		if !ok {
			t.Fatalf("repair command missing from error: %v", err)
		}
		cmd := exec.CommandContext(t.Context(), "bash", "-c", command) //nolint:gosec // generated command is the value under test.
		cmd.Dir = t.TempDir()
		if out, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("repair command failed outside mapped repo: %v\n%s", runErr, out)
		}
		if got := runTestGit(t, t.Context(), hostDir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "main@{upstream}"); got != "origin/main" {
			t.Fatalf("repaired upstream = %q, want origin/main", got)
		}
	})
	t.Run("host_upstream_repair_requires_usable_default", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name        string
			configure   func(*testing.T, string)
			wantProblem string
		}{
			{
				name:        "recorded_remote_missing",
				configure:   func(*testing.T, string) {},
				wantProblem: "no git remotes configured",
			},
			{
				name: "remote_tracking_ref_missing",
				configure: func(t *testing.T, dir string) {
					runTestGit(t, t.Context(), dir, "remote", "add", "origin", "/dev/null")
				},
				wantProblem: "does not exist",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				runTestGit(t, t.Context(), dir, "init", "-q", "--initial-branch=main")
				runTestGit(t, t.Context(), dir, "commit", "-q", "--allow-empty", "-m", "main")
				test.configure(t, dir)
				repo := Repo{
					GitRoot:       dir,
					Branches:      []string{"main"},
					Remotes:       []string{"origin"},
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}

				_, _, err := repo.requiredMappedBranchUpstream(t.Context(), testLogger(t), "main")
				if err == nil || !strings.Contains(err.Error(), test.wantProblem) || !strings.Contains(err.Error(), "choose a remote branch") {
					t.Fatalf("requiredMappedBranchUpstream error = %v, want %q and explicit choice", err, test.wantProblem)
				}
				if strings.Contains(err.Error(), "run:") {
					t.Fatalf("error suggests an unusable command: %v", err)
				}
			})
		}
	})
	t.Run("diff_guides_mapped_host_branch_repairs", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		for _, test := range []struct {
			name        string
			setup       func(*testing.T, *Container, string, string)
			wantProblem string
			commandArgs []string
		}{
			{
				name: "deleted_branch",
				setup: func(t *testing.T, _ *Container, hostDir, _ string) {
					runTestGit(t, t.Context(), hostDir, "switch", "-q", "--detach")
					runTestGit(t, t.Context(), hostDir, "branch", "-D", "main")
				},
				wantProblem: `mapped host branch "main" no longer exists`,
				commandArgs: []string{"branch", "--track", "main", "origin/main"},
			},
			{
				name: "incomplete_upstream",
				setup: func(t *testing.T, ct *Container, hostDir, _ string) {
					ct.Repos[0].Remotes = nil
					ct.Repos[0].DefaultRemote = ""
					ct.Repos[0].DefaultBranch = ""
					runTestGit(t, t.Context(), hostDir, "config", "--unset", "branch.main.merge")
				},
				wantProblem: `incomplete upstream configuration for branch "main"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
			{
				name: "invalid_upstream",
				setup: func(t *testing.T, ct *Container, hostDir, _ string) {
					ct.Repos[0].Remotes = nil
					ct.Repos[0].DefaultRemote = ""
					ct.Repos[0].DefaultBranch = ""
					runTestGit(t, t.Context(), hostDir, "config", "branch.main.merge", "refs/tags/main")
				},
				wantProblem: `invalid upstream ref for branch "main": "refs/tags/main"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
			{
				name: "md_transport_upstream",
				setup: func(t *testing.T, ct *Container, hostDir, _ string) {
					runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", "md-test")
					runTestGit(t, t.Context(), hostDir, "remote", "set-url", "md-test", "user@md-test:"+ct.Repos[0].ContainerPath)
				},
				wantProblem: `mapped host branch "main" tracks md container remote "md-test"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
			{
				name: "missing_local_upstream",
				setup: func(t *testing.T, _ *Container, hostDir, containerDir string) {
					for _, dir := range []string{hostDir, containerDir} {
						runTestGit(t, t.Context(), dir, "config", "branch.main.remote", ".")
						runTestGit(t, t.Context(), dir, "config", "branch.main.merge", "refs/heads/missing")
					}
				},
				wantProblem: `mapped host branch "main" tracks missing local branch "missing"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
		} {
			t.Run(test.name, func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
				ct, hostDir, containerDir := setupPullTest(t)
				test.setup(t, ct, hostDir, containerDir)
				err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
				wantCommand := shellQuoteArgs(append([]string{"git", "-C", hostDir}, test.commandArgs...))
				if err == nil || !strings.Contains(err.Error(), test.wantProblem) || !strings.Contains(err.Error(), wantCommand) {
					t.Fatalf("Diff error = %v, want problem %q and command %q", err, test.wantProblem, wantCommand)
				}
			})
		}
	})
	t.Run("diff_guides_secondary_branch_away_from_primary", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupMultiBranchPullTest(t)
		runTestGit(t, t.Context(), hostDir, "config", "branch.feature.remote", ".")
		runTestGit(t, t.Context(), hostDir, "config", "branch.feature.merge", "refs/heads/main")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		wantCommand := shellQuoteArgs([]string{"git", "-C", hostDir, "branch", "--set-upstream-to=origin/main", "feature"})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) || !strings.Contains(err.Error(), wantCommand) {
			t.Fatalf("Diff error = %v, want local-primary repair command", err)
		}
	})
	t.Run("diff_resolves_legacy_defaults_before_host_upstream_validation", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		ct.Repos[0].Remotes = nil
		ct.Repos[0].DefaultRemote = ""
		ct.Repos[0].DefaultBranch = ""
		runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", "md-test")
		runTestGit(t, t.Context(), hostDir, "remote", "set-url", "md-test", "user@md-test:"+ct.Repos[0].ContainerPath)

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		wantCommand := shellQuoteArgs([]string{"git", "-C", hostDir, "branch", "--set-upstream-to=origin/main", "main"})
		if err == nil || !strings.Contains(err.Error(), wantCommand) {
			t.Fatalf("Diff error = %v, want resolved legacy default repair command", err)
		}
		if strings.Contains(err.Error(), "--set-upstream-to=/") {
			t.Fatalf("Diff error contains malformed repair command: %v", err)
		}
	})
	t.Run("diff_preserves_ambiguous_legacy_default_diagnostic", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, t.Context(), hostDir, "remote", "rename", "origin", "first")
		runTestGit(t, t.Context(), hostDir, "remote", "add", "second", "/dev/null")
		runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", "md-test")
		runTestGit(t, t.Context(), hostDir, "remote", "set-url", "md-test", "user@md-test:"+ct.Repos[0].ContainerPath)
		ct.Repos[0].Remotes = nil
		ct.Repos[0].DefaultRemote = ""
		ct.Repos[0].DefaultBranch = ""

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), `multiple remotes and no "origin"`) {
			t.Fatalf("Diff error = %v, want ambiguous default diagnostic", err)
		}
		if strings.Contains(err.Error(), "--set-upstream-to=/") {
			t.Fatalf("Diff error contains malformed repair command: %v", err)
		}
	})
	t.Run("diff_reports_stopped_container", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + "=exited",
		}
		ct := &Container{
			Client: &Client{Home: t.TempDir(), Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos:  []Repo{{Branches: []string{"main"}}},
		}
		err = ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		if err == nil || err.Error() != "Container md-test is stopped. Restart it with: md start" {
			t.Fatalf("Diff error = %v, want stopped-container guidance", err)
		}
	})
	t.Run("container_state_policy", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name      string
			state     string
			policy    containerStatePolicy
			wantError string
		}{
			{name: "running_requires_running", state: "running", policy: containerMustBeRunning},
			{name: "running_allows_stopped", state: "running", policy: containerMayBeStopped},
			{name: "exited_requires_running", state: "exited", policy: containerMustBeRunning, wantError: "Container md-test is stopped. Restart it with: md start"},
			{name: "exited_allows_stopped", state: "exited", policy: containerMayBeStopped},
			{name: "paused_requires_running", state: "paused", policy: containerMustBeRunning, wantError: "Container md-test is stopped. Restart it with: md start"},
			{name: "paused_rejects_stopped", state: "paused", policy: containerMayBeStopped, wantError: "Container md-test is paused. Fork requires a running or stopped container"},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				writeTestSSHConfig(t, home)
				logger := testLogger(t)
				env := []string{
					fakeRuntimeEnv + "=1",
					fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
					fakeRuntimeStateEnv + "=" + test.state,
				}
				ct := &Container{
					Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
					Logger: logger,
					Name:   "md-test",
					State:  "running",
				}
				err := ct.checkContainerState(t.Context(), test.policy)
				if test.wantError == "" && err != nil {
					t.Fatalf("checkContainerState() = %v, want nil", err)
				}
				if test.wantError != "" && (err == nil || err.Error() != test.wantError) {
					t.Fatalf("checkContainerState() = %v, want %q", err, test.wantError)
				}
			})
		}
	})
	t.Run("container_state_surfaces_inspect_parse_error", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + "=raw:not-json",
		}
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			State:  "running",
		}
		err = ct.checkContainerState(t.Context(), containerMustBeRunning)
		if err == nil || !strings.Contains(err.Error(), `inspecting container "md-test"`) || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("checkContainerState() = %v, want inspect parse error", err)
		}
	})
	t.Run("container_state_checks_every_repo_remote", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		firstRepo := t.TempDir()
		secondRepo := t.TempDir()
		for _, dir := range []string{firstRepo, secondRepo} {
			runTestGit(t, t.Context(), dir, "init", "-q")
		}
		runTestGit(t, t.Context(), firstRepo, "remote", "add", "md-test", "/dev/null")
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + "=running",
		}
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos:  []Repo{{GitRoot: firstRepo}, {GitRoot: secondRepo}},
		}
		err = ct.checkContainerState(t.Context(), containerMustBeRunning)
		want := fmt.Sprintf("Git remote is missing from %q", secondRepo)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("checkContainerState() = %v, want %q", err, want)
		}
	})
	t.Run("diff_preserves_runtime_inspection_failure", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeErrorEnv + "=permission denied connecting to runtime socket",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		}
		ct := &Container{
			Client: &Client{Home: t.TempDir(), Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos:  []Repo{{Branches: []string{"main"}}},
		}
		err = ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), `inspecting container "md-test"`) || !strings.Contains(err.Error(), "permission denied connecting to runtime socket") {
			t.Fatalf("Diff error = %v, want runtime inspection diagnostic", err)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("Diff error = %v, want wrapped runtime exit status 1", err)
		}
	})
	t.Run("diff_preserves_upstream_ssh_failure", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "branch.main.remote")
		t.Setenv(fakeSSHFailureTextEnv, "ssh: connect to host md-test port 22: Connection refused")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), "Connection refused") || strings.Contains(err.Error(), "has no configured upstream") {
			t.Fatalf("Diff error = %v, want classified SSH failure", err)
		}
	})
	t.Run("diff_preserves_upstream_path_failure", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, _, _ := setupPullTest(t)
		missingPath := filepath.ToSlash(filepath.Join(t.TempDir(), "missing"))
		ct.Repos[0].ContainerPath = missingPath

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), missingPath) || strings.Contains(err.Error(), "has no configured upstream") {
			t.Fatalf("Diff error = %v, want classified container path failure", err)
		}
	})
	t.Run("diff_repairs_missing_and_malformed_container_upstreams", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		for _, test := range []struct {
			name  string
			setup func(*testing.T, string)
		}{
			{name: "missing", setup: func(t *testing.T, dir string) {
				runTestGit(t, t.Context(), dir, "config", "--unset", "branch.main.remote")
			}},
			{name: "malformed", setup: func(t *testing.T, dir string) {
				runTestGit(t, t.Context(), dir, "config", "branch.main.merge", "refs/tags/main")
			}},
		} {
			t.Run(test.name, func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
				ct, _, containerDir := setupPullTest(t)
				test.setup(t, containerDir)
				err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
				if err == nil || !strings.Contains(err.Error(), "run md pull or md push") {
					t.Fatalf("Diff error = %v, want upstream repair guidance", err)
				}
			})
		}
	})
	t.Run("diff_preserves_final_ssh_failure", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "GIT_OPTIONAL_LOCKS=0")
		t.Setenv(fakeSSHFailureTextEnv, "ssh: connection reset by peer")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), `running diff in container "md-test" over SSH`) || !strings.Contains(err.Error(), "connection reset by peer") {
			t.Fatalf("Diff error = %v, want final SSH diagnostic", err)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 255 {
			t.Fatalf("Diff error = %v, want wrapped SSH exit status 255", err)
		}
	})
	t.Run("diff_preserves_final_ssh_exit_one_diagnostic", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "GIT_OPTIONAL_LOCKS=0")
		t.Setenv(fakeSSHFailureStatusEnv, "1")
		t.Setenv(fakeSSHFailureTextEnv, "fatal: simulated remote Git failure")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, 0, &DiffOpts{Args: []string{"--exit-code"}})
		if err == nil || errors.Is(err, ErrDiffFound) || !strings.Contains(err.Error(), `running diff in container "md-test" over SSH`) || !strings.Contains(err.Error(), "fatal: simulated remote Git failure") {
			t.Fatalf("Diff error = %v, want wrapped remote Git diagnostic", err)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("Diff error = %v, want wrapped SSH exit status 1", err)
		}
	})
	t.Run("diff_forwards_stderr_when_differences_are_found", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "GIT_OPTIONAL_LOCKS=0")
		t.Setenv(fakeSSHFailureStatusEnv, strconv.Itoa(diffFoundSSHExitCode))
		t.Setenv(fakeSSHFailureTextEnv, "warning: simulated Git warning")
		var stderr bytes.Buffer

		err := ct.Diff(t.Context(), io.Discard, &stderr, 0, &DiffOpts{Args: []string{"--quiet"}})
		if !errors.Is(err, ErrDiffFound) {
			t.Fatalf("Diff error = %v, want ErrDiffFound", err)
		}
		if got := strings.TrimSpace(stderr.String()); got != "warning: simulated Git warning" {
			t.Fatalf("Diff stderr = %q, want forwarded Git warning", got)
		}
	})
	t.Run("diff_rejects_changed_host_branch_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "push", "-q", "origin", "main:release")
		runTestGit(t, ctx, hostDir, "fetch", "-q", "origin", "release")
		runTestGit(t, ctx, hostDir, "branch", "--set-upstream-to=origin/release", "main")

		err := ct.Diff(ctx, io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), `upstream changed from "origin/main" in the container to "origin/release" on the host`) || !strings.Contains(err.Error(), "run md pull or md push") {
			t.Fatalf("Diff error = %v, want changed upstream guidance", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.merge"); got != "refs/heads/main" {
			t.Fatalf("container branch merge ref after Diff = %q, want unchanged refs/heads/main", got)
		}
		if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
			t.Fatalf("Pull after upstream change: %v", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.remote"); got != "origin" {
			t.Errorf("container branch remote = %q, want origin", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.merge"); got != "refs/heads/release" {
			t.Errorf("container branch merge ref = %q, want refs/heads/release", got)
		}
	})
	t.Run("diff_rejects_changed_extra_branch_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupMultiBranchPullTest(t)
		runTestGit(t, ctx, containerDir, "switch", "-q", "-c", "feature", "origin/feature")
		runTestGit(t, ctx, hostDir, "push", "-q", "origin", "feature:release")
		runTestGit(t, ctx, hostDir, "fetch", "-q", "origin", "release")
		runTestGit(t, ctx, hostDir, "branch", "--set-upstream-to=origin/release", "feature")

		err := ct.Diff(ctx, io.Discard, io.Discard, 0, nil)
		if err == nil || !strings.Contains(err.Error(), `mapped branch "feature" upstream changed`) || !strings.Contains(err.Error(), "run md pull or md push") {
			t.Fatalf("Diff error = %v, want changed extra-branch upstream guidance", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.feature.merge"); got != "refs/heads/feature" {
			t.Fatalf("container feature merge ref after Diff = %q, want unchanged refs/heads/feature", got)
		}
	})
	t.Run("diff_syncs_local_branch_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "branch", "migration", "origin/main")
		runTestGit(t, ctx, hostDir, "config", "branch.main.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.main.merge", "refs/heads/migration")

		if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
			t.Fatalf("Pull after local upstream change: %v", err)
		}
		var stdout bytes.Buffer
		if err := ct.Diff(ctx, &stdout, io.Discard, 0, &DiffOpts{Args: []string{"--name-only"}, Full: true}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "shared.txt" {
			t.Fatalf("full diff --name-only = %q, want shared.txt", got)
		}
		stdout.Reset()
		if err := ct.Diff(ctx, &stdout, io.Discard, 0, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "" {
			t.Fatalf("diff --name-only after pull = %q, want no change since the sync point", got)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", "migration"); got != runTestGit(t, ctx, hostDir, "rev-parse", "migration") {
			t.Errorf("container migration = %q, want host migration", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.remote"); got != "." {
			t.Errorf("container branch remote = %q, want .", got)
		}
	})
	t.Run("diff_reports_container_work_since_the_last_pull", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, _, containerDir := setupPullTest(t)
		syncPoint := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
		if want := runTestGit(t, ctx, containerDir, "rev-parse", "main"); syncPoint != want {
			t.Fatalf("sync point after pull = %q, want container main %q", syncPoint, want)
		}
		writeTestFile(t, filepath.Join(containerDir, "turn.txt"), "turn\n")
		runTestGit(t, ctx, containerDir, "add", ".")
		runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "turn")

		var stdout bytes.Buffer
		if err := ct.Diff(ctx, &stdout, io.Discard, 0, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "turn.txt" {
			t.Fatalf("diff --name-only = %q, want turn.txt", got)
		}
		// Amending the branch tip must not move the base of the diff.
		writeTestFile(t, filepath.Join(containerDir, "amended.txt"), "amended\n")
		runTestGit(t, ctx, containerDir, "add", ".")
		runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "--no-edit")
		stdout.Reset()
		if err := ct.Diff(ctx, &stdout, io.Discard, 0, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(stdout.String()), "amended.txt\nturn.txt"; got != want {
			t.Fatalf("diff --name-only after amend = %q, want %q", got, want)
		}
		stdout.Reset()
		if err := ct.Diff(ctx, &stdout, io.Discard, 0, &DiffOpts{Args: []string{"--name-only"}, Full: true}); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(stdout.String()), "amended.txt\nshared.txt\nturn.txt"; got != want {
			t.Fatalf("full diff --name-only = %q, want %q", got, want)
		}
	})
	t.Run("fetch_without_commit_preserves_unintegrated_diff", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		previousSync := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
		writeTestFile(t, filepath.Join(containerDir, "committed.txt"), "committed\n")
		runTestGit(t, ctx, containerDir, "add", ".")
		runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "committed work")
		containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
		writeTestFile(t, filepath.Join(containerDir, "pending.txt"), "pending\n")

		fetched, err := ct.Fetch(ctx, io.Discard, io.Discard, 0, nil)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if want := []FetchedBranch{{BranchName: "main", CommitHash: containerTip}}; !reflect.DeepEqual(fetched, want) {
			t.Errorf("Fetch result = %+v, want %+v", fetched, want)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerTip {
			t.Errorf("container main = %q, want the tip %q it had before the fetch", got, containerTip)
		}
		if got := runTestGit(t, ctx, containerDir, "status", "--porcelain"); got != "?? pending.txt" {
			t.Errorf("container status = %q, want pending.txt still uncommitted", got)
		}
		if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != containerTip {
			t.Errorf("host tracking ref = %q, want the committed tip %q", got, containerTip)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main"); got != previousSync {
			t.Errorf("sync point = %q, want unchanged %q", got, previousSync)
		}
		// Fetching makes the commits durable on the host without acknowledging
		// them as integrated into its branch.
		var stdout bytes.Buffer
		if err := ct.Diff(ctx, &stdout, io.Discard, 0, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(stdout.String()), "committed.txt\npending.txt"; got != want {
			t.Errorf("diff --name-only = %q, want %q", got, want)
		}
	})
	t.Run("pull_refreshes_changed_push_remote", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		publishDir := filepath.Join(t.TempDir(), "publish.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", publishDir)
		runTestGit(t, ctx, hostDir, "remote", "add", "publish", publishDir)
		runTestGit(t, ctx, hostDir, "config", "branch.main.pushRemote", "publish")

		if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
			t.Fatalf("Pull after push remote change: %v", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.pushRemote"); got != "publish" {
			t.Fatalf("container branch push remote = %q, want publish", got)
		}
	})
	t.Run("command_logs_redact_sensitive_values", func(t *testing.T) {
		t.Parallel()
		rt, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		logger := slog.New(slog.NewTextHandler(io.MultiWriter(&log, testLogWriter{t: t}), testLoggerOptions()))
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		}
		c := &Client{
			Logger:  logger,
			Runtime: testRuntime(t, rt, logger, env),
			env:     env,
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
		ct := &Container{Client: c, Logger: logger}
		if _, err := ct.runCmd(t.Context(), "", args); err != nil {
			t.Fatal(err)
		}
		if err := ct.runCmdOut(t.Context(), "", args, io.Discard, io.Discard); err != nil {
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
	t.Run("host_user_environment", func(t *testing.T) {
		t.Parallel()
		if got, want := rootlessPodmanUserNSArg(), "--userns=keep-id:uid=1000,gid=1000"; got != want {
			t.Errorf("rootlessPodmanUserNSArg() = %q, want %q", got, want)
		}
		for _, tc := range []struct {
			name           string
			uid, gid       int
			rootlessPodman bool
			want           []string
		}{
			{name: "host_identity", uid: 1001, gid: 1002, want: []string{"-e", "MD_HOST_UID=1001", "-e", "MD_HOST_GID=1002"}},
			{name: "rootless_podman", uid: 1001, gid: 1002, rootlessPodman: true, want: []string{"-e", "MD_HOST_UID=1000", "-e", "MD_HOST_GID=1000"}},
			// md running as root passes no MD_HOST_UID/MD_HOST_GID. start.sh then
			// leaves the account at the fixed contract identity, which is why the
			// specialized image owns its content 1000:1000 in that case.
			{name: "root", uid: 0, gid: 0},
			{name: "root_uid", uid: 0, gid: 1002},
			{name: "root_gid", uid: 1001, gid: 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := hostUserEnvFor(tc.uid, tc.gid, tc.rootlessPodman)
				if !slices.Equal(got, tc.want) {
					t.Errorf("hostUserEnvFor(%d, %d, %v) = %q, want %q", tc.uid, tc.gid, tc.rootlessPodman, got, tc.want)
				}
			})
		}
	})
	t.Run("runGitDir_overrides_client_environment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		gitDir := filepath.Join(t.TempDir(), "repo.git")
		runTestGit(t, t.Context(), "", "init", "-q", "--bare", gitDir)
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{
				Logger: logger,
				env: []string{
					"GIT_DIR=/missing/git-dir",
					"GIT_WORK_TREE=/missing/work-tree",
					"LANG=invalid",
				},
			},
			Logger: logger,
		}
		out, err := ct.runGitDir(t.Context(), dir, gitDir,
			"-c", `alias.show-env=!printf '%s\n' "$GIT_DIR" "$GIT_WORK_TREE" "$LANG"`, "show-env")
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Join([]string{gitDir, dir, "C"}, "\n"); out != want {
			t.Fatalf("environment = %q, want %q", out, want)
		}
	})
	t.Run("pushContainerRefs_empty", func(t *testing.T) {
		t.Parallel()
		if err := (&Container{}).pushContainerRefs(t.Context(), &Repo{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("sync_default_branch_preserves_push_diagnostic", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		hostDir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		missingContainerDir := filepath.Join(t.TempDir(), "missing-container.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
		runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, hostDir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
		runTestGit(t, ctx, hostDir, "remote", "add", "md-test", missingContainerDir)

		ct := &Container{
			Client: testClient(t),
			Logger: testLogger(t),
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       hostDir,
				Branches:      []string{"main"},
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		err := ct.SyncDefaultBranch(ctx, 0)
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(hostDir)) || !strings.Contains(err.Error(), `container "md-test"`) || !strings.Contains(err.Error(), "does not appear to be a git repository") {
			t.Fatalf("SyncDefaultBranch error = %v, want repository/container context and Git stderr", err)
		}
	})
	t.Run("sync_default_branch_contextualizes_source_ref_failure", func(t *testing.T) {
		t.Parallel()
		missingRepo := filepath.Join(t.TempDir(), "missing-repository")
		ct := &Container{
			Client: testClient(t),
			Logger: testLogger(t),
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       missingRepo,
				Branches:      []string{"main"},
				Remotes:       []string{"origin"},
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		err := ct.SyncDefaultBranch(t.Context(), 0)
		if err == nil || !strings.Contains(err.Error(), missingRepo) || !strings.Contains(err.Error(), `container "md-test"`) {
			t.Fatalf("SyncDefaultBranch error = %v, want source repository/container context", err)
		}
	})
	t.Run("pushRefspecs_skips_host_hooks", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("Git hooks require a POSIX executable in this test")
		}
		ctx := t.Context()
		hostDir := t.TempDir()
		remoteDir := filepath.Join(t.TempDir(), "container.git")
		hooksDir := filepath.Join(hostDir, "hooks")

		runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
		runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
		writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "main\n")
		runTestGit(t, ctx, hostDir, "add", ".")
		runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main")
		if err := os.Mkdir(hooksDir, 0o750); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(hooksDir, "pre-push"), "#!/bin/sh\nexit 1\n")
		// #nosec G302 -- Git requires hooks to be executable.
		if err := os.Chmod(filepath.Join(hooksDir, "pre-push"), 0o700); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, ctx, hostDir, "config", "core.hooksPath", hooksDir)

		ct := &Container{Client: testClient(t), Logger: testLogger(t)}
		if err := ct.pushRefspecs(ctx, hostDir, remoteDir, []string{"main:main"}, false, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/heads/main"); got != runTestGit(t, ctx, hostDir, "rev-parse", "main") {
			t.Errorf("pushed main = %q, want %q", got, runTestGit(t, ctx, hostDir, "rev-parse", "main"))
		}
	})
	t.Run("SyncDefaultBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("local_only_default_branch", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "migration")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "migration\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "migration")
			migrationCommit := runTestGit(t, ctx, dir, "rev-parse", "migration")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "caic-1")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "task\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "task")
			runTestGit(t, ctx, dir, "config", "branch.caic-1.remote", ".")
			runTestGit(t, ctx, dir, "config", "branch.caic-1.merge", "refs/heads/migration")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"caic-1"},
					DefaultRemote: "origin",
					DefaultBranch: "migration",
				}},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/remotes/origin/migration"); got != migrationCommit {
				t.Errorf("pushed origin/migration = %q, want %q", got, migrationCommit)
			}
			var stdout, stderr bytes.Buffer
			if _, err := ct.pushMappedBranchRefs(ctx, &stdout, &stderr, &ct.Repos[0]); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stderr.String(), "deleting a non-existent ref") {
				t.Errorf("push mapped branches emitted deletion warning:\n%s", stderr.String())
			}
		})
		t.Run("all_remote_branches", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			originDir := filepath.Join(t.TempDir(), "origin.git")
			upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
			upstreamPushDir := filepath.Join(t.TempDir(), "upstream-push.git")
			containerDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamPushDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", containerDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
			runTestGit(t, ctx, dir, "push", "-q", "origin", "main", "main:maintenance")
			runTestGit(t, ctx, dir, "remote", "set-head", "origin", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "release")
			writeTestFile(t, filepath.Join(dir, "release.txt"), "release\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "release")
			runTestGit(t, ctx, dir, "push", "-q", "upstream", "release")
			runTestGit(t, ctx, dir, "branch", "--set-upstream-to=upstream/release", "release")
			runTestGit(t, ctx, dir, "remote", "set-url", "--push", "upstream", upstreamPushDir)
			runTestGit(t, ctx, dir, "tag", "release-1")
			runTestGit(t, ctx, dir, "tag", "internal-1", "main")
			runTestGit(t, ctx, dir, "tag", "-a", "hidden-annotated", "-m", "hidden", "main")
			runTestGit(t, ctx, dir, "config", "push.followTags", "true")
			originCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/main")
			maintenanceCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/maintenance")
			upstreamCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/upstream/release")
			releaseTag := runTestGit(t, ctx, dir, "rev-parse", "refs/tags/release-1")

			repo := Repo{GitRoot: dir, Branches: []string{"release"}, TagRegexp: "^release-"}
			logger := testLogger(t)
			if err := repo.resolveDefaults(ctx, logger); err != nil {
				t.Fatal(err)
			}
			if want := []string{"origin", "upstream"}; !slices.Equal(repo.Remotes, want) {
				t.Fatalf("remotes = %v, want %v", repo.Remotes, want)
			}

			containerWorktree := t.TempDir()
			runTestGit(t, ctx, containerWorktree, "init", "-q")
			runTestGit(t, ctx, dir, "push", "-q", containerWorktree,
				"refs/remotes/origin/main:refs/remotes/origin/main",
				"refs/remotes/upstream/release:refs/remotes/upstream/release")
			repo.ContainerPath = filepath.ToSlash(containerWorktree)
			configs, err := repo.containerRemoteConfigs(ctx, logger)
			if err != nil {
				t.Fatal(err)
			}
			commands := containerRemoteConfigCommands(&repo, configs)
			commands = append(commands, containerBranchSetupCommands(
				[]containerBranchBase{{branch: "release", ref: "upstream/release", upstreamRemote: "upstream", upstreamBranch: "release", pushRemote: "origin"}},
			)...)
			cmd := exec.CommandContext(ctx, "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // commands are generated from test temp paths
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("configure container remotes: %v\n%s", err, out)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "origin"); !sameTestPath(got, originDir) {
				t.Errorf("container origin URL = %q, want %q", got, originDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "upstream"); !sameTestPath(got, upstreamDir) {
				t.Errorf("container upstream URL = %q, want %q", got, upstreamDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "--push", "upstream"); !sameTestPath(got, upstreamPushDir) {
				t.Errorf("container upstream push URL = %q, want %q", got, upstreamPushDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); got != "upstream/release" {
				t.Errorf("container upstream = %q, want upstream/release", got)
			}
			if got := runTestGit(t, ctx, containerWorktree, "for-each-ref", "--format=%(push:remotename)", "refs/heads/release"); got != "origin" {
				t.Errorf("container push remote = %q, want origin", got)
			}
			// A removed host remote must not erase the usable container URL or
			// prevent its cached refs from being synchronized.
			runTestGit(t, ctx, dir, "config", "--remove-section", "remote.upstream")
			configs, err = repo.containerRemoteConfigs(ctx, logger)
			if err != nil {
				t.Fatal(err)
			}
			cmd = exec.CommandContext(ctx, "bash", "-c", strings.Join(containerRemoteConfigCommands(&repo, configs), " && ")) //nolint:gosec // commands are generated from test temp paths
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("refresh container remotes: %v\n%s", err, out)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "upstream"); !sameTestPath(got, upstreamDir) {
				t.Errorf("preserved upstream URL = %q, want %q", got, upstreamDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "--push", "upstream"); !sameTestPath(got, upstreamPushDir) {
				t.Errorf("preserved upstream push URL = %q, want %q", got, upstreamPushDir)
			}

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   containerDir,
				Repos:  []Repo{repo},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/remotes/origin/main"); got != originCommit {
				t.Errorf("pushed origin/main = %q, want %q", got, originCommit)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/remotes/origin/maintenance"); got != maintenanceCommit {
				t.Errorf("pushed origin/maintenance = %q, want %q", got, maintenanceCommit)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/remotes/upstream/release"); got != upstreamCommit {
				t.Errorf("pushed upstream/release = %q, want %q", got, upstreamCommit)
			}
			if _, err := (&git.Checkout{Root: containerDir, Logger: logger}).RevParse(ctx, "refs/remotes/origin/HEAD"); err == nil {
				t.Error("container origin/HEAD exists, want it omitted")
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/tags/release-1"); got != releaseTag {
				t.Errorf("pushed release-1 tag = %q, want %q", got, releaseTag)
			}
			if _, err := (&git.Checkout{Root: containerDir, Logger: logger}).RevParse(ctx, "refs/tags/internal-1"); err == nil {
				t.Error("container internal-1 tag exists, want it filtered out")
			}
			if _, err := (&git.Checkout{Root: containerDir, Logger: logger}).RevParse(ctx, "refs/tags/hidden-annotated"); err == nil {
				t.Error("container hidden-annotated tag exists via push.followTags, want it filtered out")
			}

			noTagsDir := filepath.Join(t.TempDir(), "no-tags.git")
			runTestGit(t, ctx, "", "init", "-q", "--bare", noTagsDir)
			noTagsRepo := repo
			noTagsRepo.TagRegexp = ""
			noTagsContainer := &Container{
				Client: testClient(t),
				Logger: logger,
				Name:   noTagsDir,
				Repos:  []Repo{noTagsRepo},
			}
			if err := noTagsContainer.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := (&git.Checkout{Root: noTagsDir, Logger: logger}).RevParse(ctx, "refs/tags/hidden-annotated"); err == nil {
				t.Error("default mapping included hidden-annotated tag, want no tags")
			}

			allTagsDir := filepath.Join(t.TempDir(), "all-tags.git")
			runTestGit(t, ctx, "", "init", "-q", "--bare", allTagsDir)
			allTagsRepo := repo
			allTagsRepo.TagRegexp = ".*"
			allTagsContainer := &Container{
				Client: testClient(t),
				Logger: logger,
				Name:   allTagsDir,
				Repos:  []Repo{allTagsRepo},
			}
			if err := allTagsContainer.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, allTagsDir, "rev-parse", "refs/tags/internal-1"); got != originCommit {
				t.Errorf("pushed all-tags internal-1 = %q, want %q", got, originCommit)
			}
		})
		t.Run("default_branch_also_updates_origin_ref", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			originDir := filepath.Join(t.TempDir(), "origin.git")
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "remote main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "remote main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main")
			remoteCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/main")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local main\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "local main")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"main"},
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/remotes/origin/main"); got != remoteCommit {
				t.Errorf("pushed origin/main = %q, want %q", got, remoteCommit)
			}
		})
		t.Run("tracked_branch_origin_ref_uses_upstream", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			originDir := filepath.Join(t.TempDir(), "origin.git")
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "remote feature\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "remote feature")
			runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "feature")
			remoteFeatureCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/feature")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local feature\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "local feature")
			localFeatureCommit := runTestGit(t, ctx, dir, "rev-parse", "feature")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"feature"},
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			gotFeatureCommit := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/remotes/origin/feature")
			if gotFeatureCommit != remoteFeatureCommit {
				t.Errorf("pushed origin/feature = %q, want upstream %q", gotFeatureCommit, remoteFeatureCommit)
			}
			if gotFeatureCommit == localFeatureCommit {
				t.Errorf("pushed origin/feature = local unpushed commit %q", gotFeatureCommit)
			}
		})
		t.Run("missing_mapped_branch_is_rejected", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "README.md"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "multiple_branches")
			writeTestFile(t, filepath.Join(dir, "README.md"), "deleted branch\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "deleted branch")
			runTestGit(t, ctx, dir, "checkout", "-q", "main")
			runTestGit(t, ctx, dir, "branch", "-D", "multiple_branches")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"multiple_branches"},
					DefaultRemote: "origin",
					DefaultBranch: "multiple_branches",
				}},
			}
			err := ct.SyncDefaultBranch(ctx, 0)
			if err == nil || !strings.Contains(err.Error(), `mapped host branch "multiple_branches" no longer exists`) {
				t.Fatalf("SyncDefaultBranch error = %v, want deleted host branch error", err)
			}
		})
	})
	t.Run("resolveContainerBranchBase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		upstreamDir := filepath.Join(t.TempDir(), "upstream.git")

		runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
		runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
		runTestGit(t, ctx, dir, "push", "-q", "origin", "main")
		runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", "main")
		runTestGit(t, ctx, dir, "config", "remote.pushDefault", "origin")

		// Simulate a persisted Repo label from before Remotes was added.
		repo := Repo{GitRoot: dir, Branches: []string{"main"}, DefaultRemote: "origin", DefaultBranch: "main"}
		logger := testLogger(t)
		if err := repo.resolveDefaults(ctx, logger); err != nil {
			t.Fatal(err)
		}
		if want := []string{"origin", "upstream"}; !slices.Equal(repo.Remotes, want) {
			t.Fatalf("legacy remotes = %v, want %v", repo.Remotes, want)
		}
		base, err := repo.resolveContainerBranchBase(ctx, logger, "main")
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != "upstream/main" || base.destination != "refs/remotes/upstream/main" || base.pushRemote != "origin" {
			t.Fatalf("remote base = %+v, want upstream/main with origin push remote", base)
		}
		extraBase, err := repo.resolveForkExtraBranchBase(ctx, logger, "main", "main-0")
		if err != nil {
			t.Fatal(err)
		}
		if extraBase.branch != "main-0" || extraBase.ref != containerIncomingRefPrefix+"main-0" || extraBase.destination != containerIncomingRefPrefix+"main-0" || extraBase.upstreamRemote != "upstream" || extraBase.upstreamBranch != "main" || extraBase.pushRemote != "origin" {
			t.Fatalf("fork extra base = %+v, want incoming main-0 with upstream/main and origin push remote", extraBase)
		}

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local main\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "local main")
		base, err = repo.resolveContainerBranchBase(ctx, logger, "main")
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != containerIncomingRefPrefix+"main" || base.upstreamRemote != "upstream" || base.upstreamBranch != "main" || base.destination != containerIncomingRefPrefix+"main" {
			t.Fatalf("local base = %+v, want incoming main tracking upstream/main", base)
		}
		hostTip := runTestGit(t, ctx, dir, "rev-parse", "refs/heads/main")
		containerDir := t.TempDir()
		runTestGit(t, ctx, containerDir, "init", "-q")
		runTestGit(t, ctx, dir, "push", "-q", containerDir, base.source+":"+base.destination)
		setup := exec.CommandContext(ctx, "bash", "-c", strings.Join(containerBranchSetupCommands([]containerBranchBase{base}), " && ")) //nolint:gosec // command is generated from test temp paths and fixed branch names
		setup.Dir = containerDir
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("set up branch from incoming ref: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/main"); got != hostTip {
			t.Errorf("container branch tip = %q, want host branch tip", got)
		}
		if got := runTestGit(t, ctx, containerDir, "for-each-ref", "--format=%(refname)", containerIncomingRefPrefix); got != "" {
			t.Errorf("incoming refs after branch setup = %q, want none", got)
		}

		runTestGit(t, ctx, dir, "branch", "--unset-upstream", "main")
		if _, err := repo.resolveContainerBranchBase(ctx, logger, "main"); err == nil || !strings.Contains(err.Error(), `mapped host branch "main" has no upstream`) {
			t.Fatalf("resolveContainerBranchBase error = %v, want missing upstream error", err)
		}
	})
	t.Run("Pull", func(t *testing.T) {
		t.Run("rejects_existing_rebase", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, _ := setupPullTest(t)
			previousTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")

			rebase := exec.CommandContext(ctx, "git", "rebase", "--exec", "git rev-parse --verify refs/heads/md-test-stop-rebase", "origin/main")
			rebase.Dir = hostDir
			rebase.Env = append(os.Environ(), "LANG=C")
			if out, err := rebase.CombinedOutput(); err == nil {
				t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--porcelain"); got != "" {
				t.Fatalf("pre-existing rebase status = %q, want clean", got)
			}
			rebaseHead := runTestGit(t, ctx, hostDir, "rev-parse", "HEAD")

			err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil)
			if err == nil || !strings.Contains(err.Error(), "rebase is already in progress") {
				t.Fatalf("Pull error = %v, want existing rebase error", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "HEAD"); got != rebaseHead {
				t.Fatalf("HEAD = %q, want unchanged rebase HEAD %q", got, rebaseHead)
			}
			if active, err := gitRebaseInProgress(ctx, &git.Checkout{Root: hostDir, Logger: ct.Logger}); err != nil {
				t.Fatal(err)
			} else if !active {
				t.Fatal("pre-existing rebase was aborted")
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != previousTip {
				t.Fatalf("tracking ref = %q, want unchanged %q", got, previousTip)
			}
		})
		t.Run("rejects_dirty_host_worktree", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			previousTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main")
			writeTestFile(t, filepath.Join(hostDir, "shared.txt"), "dirty host\n")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "container v2")

			err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil)
			if err == nil || !strings.Contains(err.Error(), "pending changes on branch main locally") {
				t.Fatalf("Pull error = %v, want pending changes on branch main", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != previousTip {
				t.Fatalf("tracking ref = %q, want unchanged previous container tip", got)
			}
		})
		t.Run("allows_untracked_host_files", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			untrackedPath := filepath.Join(hostDir, "scratch.txt")
			writeTestFile(t, untrackedPath, "local scratch\n")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull with untracked host file: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:container.txt"); got != "container" {
				t.Fatalf("container.txt = %q, want integrated container commit", got)
			}
			contents, err := os.ReadFile(untrackedPath) //nolint:gosec // path belongs to the test's temporary directory.
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", untrackedPath, err)
			}
			if got := string(contents); got != "local scratch\n" {
				t.Fatalf("scratch.txt = %q, want preserved untracked contents", got)
			}
		})
		t.Run("uses_fully_qualified_remote_tracking_ref", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			runTestGit(t, ctx, hostDir, "reset", "--hard", "origin/main")
			runTestGit(t, ctx, hostDir, "branch", "md-test/main", "origin/main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull with shadowing local branch: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main"); got != containerTip {
				t.Fatalf("host main = %q, want container tip %q", got, containerTip)
			}
		})
		t.Run("uses_fully_qualified_local_branch_ref", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")
			runTestGit(t, ctx, hostDir, "fetch", "-q", "md-test", "main")
			runTestGit(t, ctx, hostDir, "tag", "main", "refs/remotes/md-test/main")
			runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "other", "origin/main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull with shadowing tag: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:host.txt"); got != "host" {
				t.Fatalf("host.txt = %q, want preserved host commit", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:container.txt"); got != "container" {
				t.Fatalf("container.txt = %q, want integrated container commit", got)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "other" {
				t.Fatalf("current branch = %q, want restored other", got)
			}
		})
		t.Run("restores_detached_head", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")
			originalHead := runTestGit(t, ctx, hostDir, "rev-parse", "origin/main")
			runTestGit(t, ctx, hostDir, "switch", "-q", "--detach", originalHead)

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull from detached HEAD: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "" {
				t.Fatalf("current branch = %q, want detached HEAD", got)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "HEAD"); got != originalHead {
				t.Fatalf("HEAD = %q, want restored %q", got, originalHead)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:host.txt"); got != "host" {
				t.Fatalf("host.txt = %q, want preserved host commit", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:container.txt"); got != "container" {
				t.Fatalf("container.txt = %q, want integrated container commit", got)
			}
		})
		t.Run("leaves_conflicting_rebase_for_manual_resolution", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			previousSync := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
			writeTestFile(t, filepath.Join(hostDir, "shared.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host")
			hostTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container")
			runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "other", "origin/main")

			var stderr bytes.Buffer
			err := ct.Pull(ctx, io.Discard, &stderr, 0, nil)
			if err == nil || !strings.Contains(err.Error(), "rebasing branch main") {
				t.Fatalf("Pull error = %v, want named rebase failure; stderr:\n%s", err, stderr.String())
			}
			if active, checkErr := gitRebaseInProgress(ctx, &git.Checkout{Root: hostDir, Logger: ct.Logger}); checkErr != nil {
				t.Fatal(checkErr)
			} else if !active {
				t.Fatal("failed Pull aborted the conflicting rebase")
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "" {
				t.Fatalf("current branch = %q, want detached rebase checkout", got)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main"); got != hostTip {
				t.Fatalf("main tip = %q, want unresolved rebase branch tip %q", got, hostTip)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--porcelain", "--untracked-files=no"); got != "UU shared.txt" {
				t.Fatalf("tracked status after failed Pull = %q, want unresolved shared.txt conflict", got)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main"); got != previousSync {
				t.Fatalf("sync point after failed Pull = %q, want unchanged %q", got, previousSync)
			}
		})
		t.Run("later_branch_failure_keeps_all_integration_points", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupMultiBranchPullTest(t)
			runTestGit(t, ctx, containerDir, "branch", "feature", "origin/feature")
			previousSync := make(map[string]string, 2)
			for _, branch := range []string{"main", "feature"} {
				previousSync[branch] = runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/"+branch)
				runTestGit(t, ctx, containerDir, "update-ref", containerSyncRefPrefix+branch, previousSync[branch])
			}

			writeTestFile(t, filepath.Join(containerDir, "container-main.txt"), "container main\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container main")
			runTestGit(t, ctx, containerDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(containerDir, "feature.txt"), "container feature\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container feature")
			runTestGit(t, ctx, containerDir, "switch", "-q", "main")

			runTestGit(t, ctx, hostDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "host feature\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host feature")
			runTestGit(t, ctx, hostDir, "switch", "-q", "main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err == nil || !strings.Contains(err.Error(), "rebasing branch feature") {
				t.Fatalf("Pull error = %v, want feature rebase failure", err)
			}
			for _, branch := range []string{"main", "feature"} {
				if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+branch); got != previousSync[branch] {
					t.Errorf("%s integration point = %q, want unchanged %q", branch, got, previousSync[branch])
				}
			}
		})
		t.Run("integrates_unchanged_tracking_tip_after_host_reset", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			runTestGit(t, ctx, hostDir, "reset", "--hard", "origin/main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull after host reset: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "main"); got != containerTip {
				t.Fatalf("host main = %q, want container tip %q", got, containerTip)
			}
		})
		t.Run("fast_forwards_after_previous_pull", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container v2")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("second Pull: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:container.txt"); got != "container v2" {
				t.Fatalf("host container.txt = %q, want container v2", got)
			}
		})
		t.Run("replaces_amended_container_commit", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "container v2")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull after amend: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:shared.txt"); got != "container v2" {
				t.Fatalf("host shared.txt = %q, want container v2", got)
			}
		})
		t.Run("rebases_host_commits_after_amended_container_commit", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host change\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host change")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "container v2")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull after amend: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:shared.txt"); got != "container v2" {
				t.Fatalf("host shared.txt = %q, want container v2", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:host.txt"); got != "host change" {
				t.Fatalf("host host.txt = %q, want host change", got)
			}
		})
		t.Run("rebases_divergent_host_history_after_container_rewrite", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			runTestGit(t, ctx, hostDir, "reset", "--hard", "origin/main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "divergent host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "divergent host")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "rewritten container\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "rewritten container")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil); err != nil {
				t.Fatalf("Pull with independently divergent host history: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:shared.txt"); got != "rewritten container" {
				t.Fatalf("shared.txt = %q, want rewritten container content", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:host.txt"); got != "divergent host" {
				t.Fatalf("host.txt = %q, want preserved divergent host commit", got)
			}
		})
		t.Run("does_not_update_container_branch", func(t *testing.T) { //nolint:paralleltest // fakeSSH changes process environment.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			containerPath := filepath.ToSlash(containerDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "base\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")
			containerCommit := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRunningRuntime(t, testLogger(t))},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main"},
					ContainerPath: containerPath,
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerCommit {
				t.Fatalf("container main = %q, want unchanged %q", got, containerCommit)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:container.txt"); got != "container" {
				t.Fatalf("host container.txt = %q, want container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:host.txt"); got != "host" {
				t.Fatalf("host host.txt = %q, want host", got)
			}
		})
		t.Run("commits_uncommitted_changes", func(t *testing.T) {
			ctx := t.Context()
			fakeSSH(t)
			sshLogPath := filepath.Join(t.TempDir(), "ssh.log")
			t.Setenv(fakeSSHLogEnv, sshLogPath)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "README.md"), "base\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			runTestGit(t, ctx, containerDir, "config", "user.name", "md")
			runTestGit(t, ctx, containerDir, "config", "user.email", "<>")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRunningRuntime(t, testLogger(t))},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main"},
					ContainerPath: filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			fetched, err := ct.Fetch(ctx, &stdout, &stderr, 0, &FetchOpts{Commit: true})
			if err != nil {
				t.Fatalf("Fetch: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if len(fetched) != 1 || fetched[0].BranchName != "main" || fetched[0].CommitHash == "" {
				t.Fatalf("Fetch result = %+v, want main branch commit", fetched)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "md-test/main:container.txt"); got != "container" {
				t.Fatalf("fetched container.txt = %q, want container", got)
			}
			if got := runTestGit(t, ctx, containerDir, "show", "-s", "--format=%an <%ae>%n%cn <%ce>"); got != "Test <test@test>\nTest <test@test>" {
				t.Fatalf("container commit identity = %q, want host identity for author and committer", got)
			}
			sshLog, err := os.ReadFile(sshLogPath) //nolint:gosec // test log is under t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			sshCommands := strings.FieldsFunc(string(sshLog), func(r rune) bool { return r == '\n' })
			if len(sshCommands) > 1 {
				t.Fatalf("ssh invocations = %d, want at most 1\n%s", len(sshCommands), sshLog)
			}
			if !strings.Contains(string(sshLog), "git commit -a -q") {
				t.Fatalf("missing commit in ssh command:\n%s", sshLog)
			}
		})
		t.Run("hooks_reject_container_commit", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			if runtime.GOOS == "windows" {
				t.Skip("Git hooks require a POSIX executable in this test")
			}
			ctx := t.Context()
			ct, _, containerDir := setupPullTest(t)
			installRejectingHooks(t, containerDir)
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")

			err := ct.Pull(ctx, io.Discard, io.Discard, 0, nil)
			if err == nil || !strings.Contains(err.Error(), "committing in container") {
				t.Fatalf("Pull with rejecting hooks error = %v, want a committing error", err)
			}
		})
		t.Run("no_verify_skips_container_hooks", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			if runtime.GOOS == "windows" {
				t.Skip("Git hooks require a POSIX executable in this test")
			}
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			installRejectingHooks(t, containerDir)
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")

			if err := ct.Pull(ctx, io.Discard, io.Discard, 0, &PullOpts{NoVerify: true}); err != nil {
				t.Fatalf("Pull with NoVerify: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:container.txt"); got != "container" {
				t.Fatalf("host container.txt = %q, want container", got)
			}
		})
		t.Run("multiple_mapped_branches", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "README.md"), "main\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "feature\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "feature")
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "feature")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "main")

			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "main-container.txt"), "main container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container main")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "feature")
			writeTestFile(t, filepath.Join(containerDir, "feature-container.txt"), "feature container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container feature")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(io.MultiWriter(&log, testLogWriter{t: t}), testLoggerOptions()))
			ct := &Container{
				Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
				Logger: logger,
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main", "feature"},
					ContainerPath: filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if got := strings.Count(log.String(), `cmd="[git fetch -q md-test`); got != 1 {
				t.Fatalf("container fetch invocations = %d, want 1\n%s", got, log.String())
			}
			if !strings.Contains(log.String(), `cmd="[git fetch -q md-test +refs/heads/main:refs/remotes/md-test/main +refs/heads/feature:refs/remotes/md-test/feature]"`) {
				t.Fatalf("missing batched mapped-branch fetch\n%s", log.String())
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:main-container.txt"); got != "main container" {
				t.Fatalf("main branch container file = %q, want main container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "feature:feature-container.txt"); got != "feature container" {
				t.Fatalf("feature branch container file = %q, want feature container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "main" {
				t.Fatalf("current branch = %q, want main", got)
			}
		})
		t.Run("preserves_completed_branches_and_later_conflicting_rebase", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupMultiBranchPullTest(t)

			runTestGit(t, ctx, hostDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "host feature\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host feature")
			hostFeatureTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/feature")
			runTestGit(t, ctx, hostDir, "switch", "-q", "main")

			writeTestFile(t, filepath.Join(containerDir, "main-container.txt"), "main container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container main")
			containerMainTip := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/main")
			runTestGit(t, ctx, containerDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(containerDir, "feature.txt"), "container feature\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container feature")
			containerFeatureTip := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/feature")
			runTestGit(t, ctx, containerDir, "switch", "-q", "main")

			var stderr bytes.Buffer
			err := ct.Pull(ctx, io.Discard, &stderr, 0, nil)
			if err == nil || !strings.Contains(err.Error(), "integrating branch feature") {
				t.Fatalf("Pull error = %v, want feature integration failure; stderr:\n%s", err, stderr.String())
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main"); got != containerMainTip {
				t.Fatalf("main tip = %q, want completed integration %q", got, containerMainTip)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/feature"); got != hostFeatureTip {
				t.Fatalf("feature tip = %q, want pre-rebase host tip %q", got, hostFeatureTip)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/feature"); got != containerFeatureTip {
				t.Fatalf("feature tracking tip = %q, want fetched container tip %q", got, containerFeatureTip)
			}
			if active, checkErr := gitRebaseInProgress(ctx, &git.Checkout{Root: hostDir, Logger: ct.Logger}); checkErr != nil {
				t.Fatal(checkErr)
			} else if !active {
				t.Fatal("failed Pull aborted the feature rebase")
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "" {
				t.Fatalf("current branch = %q, want detached feature rebase checkout", got)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--porcelain", "--untracked-files=no"); got != "UU feature.txt" {
				t.Fatalf("tracked status after partial Pull = %q, want unresolved feature.txt conflict", got)
			}
		})
		t.Run("deleted_source_branch_is_rejected", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			branch := "multiple_branches"
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "README.md"), "base\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", branch)
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "feature\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "feature")
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", branch)
			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			runTestGit(t, ctx, containerDir, "checkout", "-q", branch)
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")

			runTestGit(t, ctx, hostDir, "checkout", "-q", "main")
			runTestGit(t, ctx, hostDir, "push", "-q", "origin", "--delete", branch)
			runTestGit(t, ctx, hostDir, "branch", "-D", branch)
			runTestGit(t, ctx, hostDir, "update-ref", "-d", "refs/remotes/origin/"+branch)
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRunningRuntime(t, testLogger(t))},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{branch},
					ContainerPath: filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: branch,
				}},
			}
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{name: "Diff", run: func() error { return ct.Diff(ctx, io.Discard, io.Discard, 0, nil) }},
				{name: "Pull", run: func() error { return ct.Pull(ctx, io.Discard, io.Discard, 0, nil) }},
			} {
				err := op.run()
				if err == nil || !strings.Contains(err.Error(), `mapped host branch "multiple_branches" no longer exists`) {
					t.Errorf("%s error = %v, want deleted host branch error", op.name, err)
				}
			}
		})
	})
	t.Run("Push", func(t *testing.T) {
		t.Run("rejects_staged_host_changes", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			writeTestFile(t, filepath.Join(hostDir, "staged.txt"), "staged\n")
			runTestGit(t, ctx, hostDir, "add", "staged.txt")

			if _, err := ct.Push(ctx, io.Discard, io.Discard, 0); err == nil || !strings.Contains(err.Error(), "pending changes on branch main locally") {
				t.Fatalf("Push error = %v, want pending host changes error", err)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerTip {
				t.Fatalf("container main = %q, want unchanged %q", got, containerTip)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--short"); got != "A  staged.txt" {
				t.Fatalf("host status = %q, want staged change preserved", got)
			}
		})
		t.Run("failed_reset_keeps_previous_sync_point", func(t *testing.T) {
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			previousSync := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			t.Setenv(fakeSSHFailureMatchEnv, "git switch -q -C main")
			t.Setenv(fakeSSHFailureTextEnv, "ssh: simulated reset failure")

			if _, err := ct.Push(ctx, io.Discard, io.Discard, 0); err == nil {
				t.Fatal("Push error = nil, want reset failure")
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerTip {
				t.Fatalf("container main = %q, want unchanged %q", got, containerTip)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main"); got != previousSync {
				t.Fatalf("sync point = %q, want previous %q", got, previousSync)
			}
		})
		t.Run("uses_fully_qualified_tracking_source", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, _ := setupPullTest(t)
			runTestGit(t, ctx, hostDir, "tag", "main", "origin/main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			hostTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main")

			if _, err := ct.Push(ctx, io.Discard, io.Discard, 0); err != nil {
				t.Fatalf("Push with shadowing tag: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != hostTip {
				t.Fatalf("tracking ref = %q, want host branch tip %q", got, hostTip)
			}
		})
		t.Run("backs_up_extra_branch_before_reset", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			sshConfigDir := filepath.Join(home, ".ssh", "config.d")
			if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sshConfigDir, "md-test.conf"), []byte("Host md-test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			containerPath := filepath.ToSlash(containerDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "feature host\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "feature host")
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "feature")

			runTestGit(t, ctx, "", "clone", "-q", "--no-checkout", hostDir, containerDir)
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "-B", "main", "origin/main")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "-B", "feature", "origin/feature")
			writeTestFile(t, filepath.Join(containerDir, "tracked.txt"), "feature container\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "feature container")
			extraBranchCommit := runTestGit(t, ctx, containerDir, "rev-parse", "feature")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			logger := testLogger(t)
			ct := &Container{
				Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
				Logger: logger,
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main", "feature"},
					ContainerPath: containerPath,
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			backupBranch, err := ct.Push(ctx, &stdout, &stderr, 0)
			if err != nil {
				t.Fatalf("Push: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			extraBackup := backupBranch + "-1-feature"
			if got := runTestGit(t, ctx, containerDir, "rev-parse", extraBackup); got != extraBranchCommit {
				t.Fatalf("extra branch backup = %q, want %q", got, extraBranchCommit)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "feature"); got == extraBranchCommit {
				t.Fatalf("feature branch was not reset from host")
			}
		})
	})
	t.Run("Fork", func(t *testing.T) {
		t.Run("host_branch_tracks_source_upstream", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name             string
				sourceRemote     string
				sourceMergeRef   string
				sourcePushRemote string
				wantRemote       string
				wantMergeRef     string
				wantPushRemote   string
			}{
				{name: "remote_branch", sourceRemote: "fork", sourceMergeRef: "refs/heads/main", sourcePushRemote: "origin", wantRemote: "fork", wantMergeRef: "refs/heads/main", wantPushRemote: "origin"},
				{name: "local_branch", sourceRemote: ".", sourceMergeRef: "refs/heads/main", wantRemote: ".", wantMergeRef: "refs/heads/main", wantPushRemote: "."},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					ctx := t.Context()
					dir := t.TempDir()
					runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
					runTestGit(t, ctx, dir, "config", "user.name", "Test")
					runTestGit(t, ctx, dir, "config", "user.email", "test@test")
					writeTestFile(t, filepath.Join(dir, "README.md"), "main\n")
					runTestGit(t, ctx, dir, "add", ".")
					runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
					runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
					runTestGit(t, ctx, dir, "remote", "add", "fork", "/dev/null")
					runTestGit(t, ctx, dir, "branch", "source")
					runTestGit(t, ctx, dir, "update-ref", "refs/remotes/fork/source", "source")
					runTestGit(t, ctx, dir, "config", "branch.autoSetupMerge", "always")
					runTestGit(t, ctx, dir, "config", "branch.autoSetupRebase", "always")
					if tt.sourceRemote != "" {
						runTestGit(t, ctx, dir, "config", "branch.source.remote", tt.sourceRemote)
						runTestGit(t, ctx, dir, "config", "branch.source.merge", tt.sourceMergeRef)
					}
					if tt.sourcePushRemote != "" {
						runTestGit(t, ctx, dir, "config", "branch.source.pushRemote", tt.sourcePushRemote)
					}

					repo := &Repo{GitRoot: dir, DefaultRemote: "origin", DefaultBranch: "main"}
					_, err := repo.createForkHostBranch(ctx, testLogger(t), "source-0", "fork/source", branchUpstreamSpec{remote: tt.wantRemote, branch: strings.TrimPrefix(tt.wantMergeRef, "refs/heads/"), pushRemote: tt.wantPushRemote})
					if err != nil {
						t.Fatal(err)
					}
					if got := runTestGit(t, ctx, dir, "rev-parse", "source-0"); got != runTestGit(t, ctx, dir, "rev-parse", "fork/source") {
						t.Errorf("fork tip = %q, want fork/source %q", got, runTestGit(t, ctx, dir, "rev-parse", "fork/source"))
					}
					if got := runTestGit(t, ctx, dir, "config", "--get", "branch.source-0.remote"); got != tt.wantRemote {
						t.Errorf("fork remote = %q, want %q", got, tt.wantRemote)
					}
					if got := runTestGit(t, ctx, dir, "config", "--get", "branch.source-0.merge"); got != tt.wantMergeRef {
						t.Errorf("fork merge ref = %q, want %q", got, tt.wantMergeRef)
					}
					gotPushRemote, _ := (&git.Checkout{Root: dir, Logger: testLogger(t)}).RunGit(ctx, "config", "--get", "branch.source-0.pushRemote")
					if gotPushRemote != tt.wantPushRemote {
						t.Errorf("fork push remote = %q, want %q", gotPushRemote, tt.wantPushRemote)
					}
					repo.Branches = []string{"source-0"}
					if _, err := repo.containerSyncRefspecs(ctx, testLogger(t)); err != nil {
						t.Fatalf("syncing fork host destination branch: %v", err)
					}
				})
			}
		})
	})
}

func TestRepoContainerHooksPath(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		hooksPath string
		want      string
	}{
		{name: "relative", hooksPath: "scripts/hooks", want: "scripts/hooks"},
		{name: "absolute", hooksPath: filepath.Join(t.TempDir(), "hooks")},
		{name: "home_relative", hooksPath: "~/.config/git/hooks"},
		{name: "escapes_repository", hooksPath: "../hooks"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			runTestGit(t, t.Context(), dir, "init", "-q")
			runTestGit(t, t.Context(), dir, "config", "core.hooksPath", tt.hooksPath)

			got, err := (&Repo{GitRoot: dir}).containerHooksPath(t.Context(), testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("containerHooksPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFork(t *testing.T) {
	t.Parallel()

	t.Run("host_branch_rollback", func(t *testing.T) {
		t.Parallel()
		newRepo := func(t *testing.T) (Repo, string, string) {
			dir := t.TempDir()
			runTestGit(t, t.Context(), dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, t.Context(), dir, "config", "user.name", "Test")
			runTestGit(t, t.Context(), dir, "config", "user.email", "test@test")
			runTestGit(t, t.Context(), dir, "commit", "-q", "--allow-empty", "-m", "base")
			base := runTestGit(t, t.Context(), dir, "rev-parse", "HEAD")
			runTestGit(t, t.Context(), dir, "commit", "-q", "--allow-empty", "-m", "fork")
			forkTip := runTestGit(t, t.Context(), dir, "rev-parse", "HEAD")
			return Repo{GitRoot: dir}, base, forkTip
		}

		t.Run("deletes_new_branch", func(t *testing.T) {
			t.Parallel()
			repo, _, forkTip := newRepo(t)
			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			if err := change.rollback(t.Context(), testLogger(t)); err != nil {
				t.Fatal(err)
			}
			g := &git.Checkout{Root: repo.GitRoot, Logger: testLogger(t)}
			exists, err := g.RefExists(t.Context(), "refs/heads/forked")
			if err != nil {
				t.Fatal(err)
			}
			if exists {
				t.Fatal("new fork branch still exists after rollback")
			}
			if remote, err := optionalGitConfig(t.Context(), g, "branch.forked.remote"); err != nil || remote != "" {
				t.Fatalf("fork branch remote after rollback = %q, %v; want absent", remote, err)
			}
		})

		t.Run("restores_existing_branch_and_config", func(t *testing.T) {
			t.Parallel()
			repo, base, forkTip := newRepo(t)
			runTestGit(t, t.Context(), repo.GitRoot, "branch", "forked", base)
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.remote", "old-origin")
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.merge", "refs/heads/old-main")
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.pushRemote", "old-push")

			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			g := &git.Checkout{Root: repo.GitRoot, Logger: testLogger(t)}
			if pushRemote, err := optionalGitConfig(t.Context(), g, "branch.forked.pushRemote"); err != nil || pushRemote != "" {
				t.Fatalf("fork branch push remote = %q, %v; want stale value cleared", pushRemote, err)
			}
			if err := change.rollback(t.Context(), testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "forked"); got != base {
				t.Fatalf("restored branch tip = %q, want %q", got, base)
			}
			for _, entry := range []struct {
				key  string
				want string
			}{
				{key: "branch.forked.merge", want: "refs/heads/old-main"},
				{key: "branch.forked.pushRemote", want: "old-push"},
				{key: "branch.forked.remote", want: "old-origin"},
			} {
				if got := runTestGit(t, t.Context(), repo.GitRoot, "config", "--get", entry.key); got != entry.want {
					t.Errorf("%s = %q, want %q", entry.key, got, entry.want)
				}
			}
		})

		t.Run("preserves_concurrently_moved_branch", func(t *testing.T) {
			t.Parallel()
			repo, _, forkTip := newRepo(t)
			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			runTestGit(t, t.Context(), repo.GitRoot, "commit", "-q", "--allow-empty", "-m", "concurrent")
			concurrentTip := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "HEAD")
			runTestGit(t, t.Context(), repo.GitRoot, "branch", "-f", "forked", concurrentTip)

			err = change.rollback(t.Context(), testLogger(t))
			if err == nil || !strings.Contains(err.Error(), "changed after fork wrote it") {
				t.Fatalf("rollback error = %v, want concurrent-change error", err)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "forked"); got != concurrentTip {
				t.Fatalf("concurrently moved branch tip = %q, want preserved %q", got, concurrentTip)
			}
		})

		t.Run("preserves_concurrently_changed_config", func(t *testing.T) {
			t.Parallel()
			repo, _, forkTip := newRepo(t)
			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.remote", "concurrent-origin")

			err = change.rollback(t.Context(), testLogger(t))
			if err == nil || !strings.Contains(err.Error(), "configuration changed after fork wrote it") {
				t.Fatalf("rollback error = %v, want concurrent-config error", err)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "forked"); got != forkTip {
				t.Fatalf("fork branch tip = %q, want preserved %q", got, forkTip)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "config", "--get", "branch.forked.remote"); got != "concurrent-origin" {
				t.Fatalf("fork branch remote = %q, want concurrent-origin", got)
			}
		})
	})

	t.Run("failed_attempt_rolls_back_artifacts", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		tip := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		const forkName = "md-repo-forked"
		runTestGit(t, ctx, dir, "remote", "add", forkName, "/dev/null")
		change, err := (&Repo{GitRoot: dir}).createForkHostBranch(ctx, testLogger(t), "forked", tip, branchUpstreamSpec{remote: "origin", branch: "main"})
		if err != nil {
			t.Fatal(err)
		}

		home := t.TempDir()
		configDir := filepath.Join(home, ".ssh", "config.d")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{forkName + ".conf", forkName + ".known_hosts"} {
			if err := os.WriteFile(filepath.Join(configDir, name), []byte("test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + logPath,
			fakeRuntimeStateEnv + "=running",
		}
		logger := testLogger(t)
		client := &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env}
		source := &Container{Client: client, Logger: logger, Name: "md-source"}
		fork := &Container{
			Client: client,
			Logger: logger,
			Name:   forkName,
			Repos:  []Repo{{GitRoot: dir}},
			State:  "running",
		}
		attempt := forkAttempt{
			source:          source,
			fork:            fork,
			snapshotImage:   "md-fork-md-source",
			hostBranches:    []forkHostBranchChange{change},
			snapshotCreated: true,
		}
		if err := attempt.rollback(ctx); err != nil {
			t.Fatal(err)
		}

		g := &git.Checkout{Root: dir, Logger: logger}
		exists, err := g.RefExists(ctx, "refs/heads/forked")
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("fork host branch still exists")
		}
		if remotes := runTestGit(t, ctx, dir, "remote"); strings.Contains(remotes, forkName) {
			t.Errorf("fork remote still exists in %q", remotes)
		}
		for _, name := range []string{forkName + ".conf", forkName + ".known_hosts"} {
			if _, err := os.Stat(filepath.Join(configDir, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("SSH artifact %s still exists: %v", name, err)
			}
		}
		logData, err := os.ReadFile(logPath) //nolint:gosec // private test log.
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"inspect " + forkName, "rm -f -v " + forkName, "rmi md-fork-md-source"} {
			if !strings.Contains(string(logData), want) {
				t.Errorf("runtime log missing %q:\n%s", want, logData)
			}
		}
	})

	t.Run("local_primary_upstream_fails_before_runtime_mutation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main")
		runTestGit(t, ctx, dir, "branch", "feature")
		runTestGit(t, ctx, dir, "config", "branch.feature.remote", ".")
		runTestGit(t, ctx, dir, "config", "branch.feature.merge", "refs/heads/main")
		runTestGit(t, ctx, dir, "remote", "add", "md-test", "/dev/null")

		home := t.TempDir()
		writeTestSSHConfig(t, home)
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{fakeRuntimeEnv + "=1", fakeRuntimeLogEnv + "=" + logPath}
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, exe, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       dir,
				Branches:      []string{"main", "feature"},
				ContainerPath: "/home/user/src/repo",
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		_, err = ct.Fork(ctx, io.Discard, io.Discard, &ForkOpts{
			Quiet: true,
			Repos: []ForkRepo{{GitRoot: dir, DestPrimary: "main-0"}},
		})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) {
			t.Fatalf("Fork error = %v, want local primary-upstream error", err)
		}
		logData, readErr := os.ReadFile(logPath) //nolint:gosec // private test log.
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(logData), "commit") || strings.Contains(string(logData), "create") || strings.Contains(string(logData), "run") {
			t.Fatalf("Fork mutated runtime before preflight failure:\n%s", logData)
		}
	})

	t.Run("missing_branch_fails_before_runtime_mutation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "md-test", "/dev/null")
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{fakeRuntimeEnv + "=1", fakeRuntimeLogEnv + "=" + logPath}
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, exe, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       dir,
				Branches:      []string{"missing"},
				ContainerPath: "/home/user/src/repo",
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		_, err = ct.Fork(ctx, io.Discard, io.Discard, &ForkOpts{
			Quiet: true,
			Repos: []ForkRepo{{GitRoot: dir, DestPrimary: "missing-0"}},
		})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "missing" no longer exists`) {
			t.Fatalf("Fork error = %v, want missing branch preflight error", err)
		}
		logData, readErr := os.ReadFile(logPath) //nolint:gosec // private test log.
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(logData), "commit") || strings.Contains(string(logData), "create") || strings.Contains(string(logData), "run") {
			t.Fatalf("fork mutated runtime before preflight failure:\n%s", logData)
		}
	})

	t.Run("valid_options_do_not_inherit_privileges", func(t *testing.T) {
		t.Parallel()
		got := (&ForkOpts{}).startOptions()
		if got.Display || got.Tailscale || got.USB || got.Sudo {
			t.Fatalf("fork options inherited disabled features: display=%v tailscale=%v usb=%v sudo=%v", got.Display, got.Tailscale, got.USB, got.Sudo)
		}
		for _, want := range []string{"MD_DISPLAY=", "MD_TAILSCALE=", "TAILSCALE_AUTHKEY=", "MD_SUDO_PASSWORD="} {
			if !slices.Contains(got.ExtraRunArgs, want) {
				t.Fatalf("fork run args missing %q in %v", want, got.ExtraRunArgs)
			}
		}

		got = (&ForkOpts{Display: true, Tailscale: true, USB: true, Sudo: true}).startOptions()
		if !got.Display || !got.Tailscale || !got.USB || !got.Sudo {
			t.Fatalf("fork options did not enable requested features: display=%v tailscale=%v usb=%v sudo=%v", got.Display, got.Tailscale, got.USB, got.Sudo)
		}
		if len(got.ExtraRunArgs) != 0 {
			t.Fatalf("enabled fork has env-clearing run args: %v", got.ExtraRunArgs)
		}
	})

	t.Run("valid_primary_branch_setup_renames_branch", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "caic-23")

		setup := exec.CommandContext(ctx, "bash", "-c", forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")) //nolint:gosec // command is generated from test branch names
		setup.Dir = dir
		setup.Env = append(os.Environ(), "LANG=C")
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("fork primary branch setup: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "branch", "--show-current"); got != "caic-23-0" {
			t.Fatalf("current branch = %q, want caic-23-0", got)
		}
	})

	t.Run("valid_sync_refspecs_pair_every_branch", func(t *testing.T) {
		t.Parallel()
		got := branchSyncRefspecs([]string{"main", "topic"}, []string{"main-0", "topic-0"})
		want := []string{
			"+refs/heads/main:" + containerSyncRefPrefix + "main-0",
			"+refs/heads/topic:" + containerSyncRefPrefix + "topic-0",
		}
		if !slices.Equal(got, want) {
			t.Errorf("branchSyncRefspecs() = %q, want %q", got, want)
		}
	})
	t.Run("valid_fork_refreshes_sync_points_to_committed_tips", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-23")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")
		staleSyncCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"caic-23", staleSyncCommit)
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "committed in source")
		primaryTip := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "branch", "topic")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"topic", staleSyncCommit)

		commands := make([]string, 1, 4)
		commands[0] = forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")
		commands = append(commands, forkSyncPointCommands("caic-23", []string{"caic-23-0", "topic"})...)
		rename := exec.CommandContext(ctx, "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // commands are generated from test branch names
		rename.Dir = dir
		rename.Env = append(os.Environ(), "LANG=C")
		if out, err := rename.CombinedOutput(); err != nil {
			t.Fatalf("fork sync point refresh: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"caic-23-0"); got != primaryTip {
			t.Errorf("primary sync point = %q, want committed tip %q", got, primaryTip)
		}
		if got := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"topic"); got != primaryTip {
			t.Errorf("topic sync point = %q, want committed tip %q", got, primaryTip)
		}
		if got := runTestGit(t, ctx, dir, "for-each-ref", "--format=%(refname)", containerSyncRefPrefix+"caic-23"); got != "" {
			t.Errorf("old primary sync ref = %q, want none", got)
		}
	})
	t.Run("valid_fork_creates_sync_point_without_a_recorded_point", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-23")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")

		commands := make([]string, 1, 3)
		commands[0] = forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")
		commands = append(commands, forkSyncPointCommands("caic-23", []string{"caic-23-0"})...)
		rename := exec.CommandContext(ctx, "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // commands are generated from test branch names
		rename.Dir = dir
		rename.Env = append(os.Environ(), "LANG=C")
		if out, err := rename.CombinedOutput(); err != nil {
			t.Fatalf("fork sync point refresh without a recorded point: %v\n%s", err, out)
		}
		if got, want := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"caic-23-0"), runTestGit(t, ctx, dir, "rev-parse", "refs/heads/caic-23-0"); got != want {
			t.Errorf("new sync point = %q, want committed tip %q", got, want)
		}
	})
	t.Run("valid_primary_branch_setup_preserves_rebase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "caic-23")
		runTestGit(t, ctx, dir, "config", "branch.caic-23.remote", "origin")
		runTestGit(t, ctx, dir, "config", "branch.caic-23.merge", "refs/heads/main")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "source\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "source")
		sourceTip := runTestGit(t, ctx, dir, "rev-parse", "refs/heads/caic-23")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "target\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "target")
		runTestGit(t, ctx, dir, "checkout", "-q", "caic-23")

		rebase := exec.CommandContext(ctx, "git", "rebase", "main")
		rebase.Dir = dir
		rebase.Env = append(os.Environ(), "LANG=C")
		if out, err := rebase.CombinedOutput(); err == nil {
			t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
		}

		rename := exec.CommandContext(ctx, "git", "branch", "-m", "caic-23", "caic-23-0")
		rename.Dir = dir
		rename.Env = append(os.Environ(), "LANG=C")
		if out, err := rename.CombinedOutput(); err == nil {
			t.Fatalf("git branch -m unexpectedly succeeded:\n%s", out)
		}

		setup := exec.CommandContext(ctx, "bash", "-c", forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")) //nolint:gosec // command is generated from test branch names
		setup.Dir = dir
		setup.Env = append(os.Environ(), "LANG=C")
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("fork primary branch setup: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "rev-parse", "refs/heads/caic-23-0"); got != sourceTip {
			t.Fatalf("new branch tip = %q, want %q", got, sourceTip)
		}
		verifyOld := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "refs/heads/caic-23") //nolint:gosec // dir is a test temp repo
		verifyOld.Env = append(os.Environ(), "LANG=C")
		if out, err := verifyOld.CombinedOutput(); err == nil {
			t.Fatalf("old branch still exists:\n%s", out)
		}
		headName, err := os.ReadFile(filepath.Join(dir, ".git", "rebase-merge", "head-name")) //nolint:gosec // test temp repo metadata
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(headName)); got != "refs/heads/caic-23-0" {
			t.Fatalf("rebase head-name = %q, want refs/heads/caic-23-0", got)
		}

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "resolved\n")
		runTestGit(t, ctx, dir, "add", ".")
		cont := exec.CommandContext(ctx, "git", "rebase", "--continue")
		cont.Dir = dir
		cont.Env = append(os.Environ(), "LANG=C", "GIT_EDITOR=true")
		if out, err := cont.CombinedOutput(); err != nil {
			t.Fatalf("git rebase --continue: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "branch", "--show-current"); got != "caic-23-0" {
			t.Fatalf("current branch after rebase = %q, want caic-23-0", got)
		}
		if got := runTestGit(t, ctx, dir, "config", "--get", "branch.caic-23-0.remote"); got != "origin" {
			t.Errorf("fork remote = %q, want origin", got)
		}
		if got := runTestGit(t, ctx, dir, "config", "--get", "branch.caic-23-0.merge"); got != "refs/heads/main" {
			t.Errorf("fork merge ref = %q, want refs/heads/main", got)
		}
	})

	t.Run("valid_snapshot_clears_runtime_state", func(t *testing.T) {
		t.Parallel()
		changes := forkSnapshotConfigChanges("md.sudo md.sudo-password md.image_type custom.label")
		for _, want := range []string{
			"LABEL md.sudo=",
			"LABEL md.sudo-password=",
			"LABEL custom.label=",
			"ENV MD_HOST_GID=",
			"ENV MD_HOST_UID=",
			"ENV MD_SUDO_PASSWORD=",
			"ENV MD_TAILSCALE=",
			"ENV TAILSCALE_AUTHKEY=",
			"LABEL md.image_type=fork-snapshot",
		} {
			if !slices.Contains(changes, want) {
				t.Fatalf("fork snapshot changes missing %q in %v", want, changes)
			}
		}
		// The fork-snapshot stamp must come after the inherited md.image_type is
		// cleared, otherwise the specialized value would win.
		cleared := slices.Index(changes, "LABEL md.image_type=")
		stamp := slices.Index(changes, "LABEL md.image_type=fork-snapshot")
		if cleared == -1 || stamp <= cleared {
			t.Fatalf("fork-snapshot stamp (%d) must follow the clearing change (%d): %v", stamp, cleared, changes)
		}
	})

	t.Run("valid_untags_image", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name string
			rt   string
			want string
		}{
			{name: "docker", rt: "docker", want: "rmi -f --no-prune md-fork-md-source"},
			{name: "podman", rt: "podman", want: "image untag md-fork-md-source"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				runtimeName := tt.rt
				if runtime.GOOS == "windows" {
					runtimeName += ".exe"
				}
				runtimePath := filepath.Join(dir, runtimeName)
				if err := linkOrCopyExecutable(exe, runtimePath); err != nil {
					t.Fatal(err)
				}
				logPath := filepath.Join(t.TempDir(), "runtime.log")
				logger := testLogger(t)
				env := []string{
					fakeRuntimeEnv + "=1",
					fakeRuntimeLogEnv + "=" + logPath,
				}
				ct := &Container{Client: &Client{
					Logger:  logger,
					Runtime: testRuntime(t, runtimePath, logger, env),
					env:     env,
				}}
				if err := ct.untagImage(t.Context(), "md-fork-md-source"); err != nil {
					t.Fatal(err)
				}
				logData, err := os.ReadFile(logPath) //nolint:gosec // logPath is a private test temp file.
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSpace(string(logData)); got != tt.want {
					t.Fatalf("runtime command = %q, want %q", got, tt.want)
				}
			})
		}
	})
}

func TestUnmarshalContainer(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if ct.Name != "md-repo-main" {
			t.Errorf("ContainerName = %q, want %q", ct.Name, "md-repo-main")
		}
		if ct.State != "running" {
			t.Errorf("State = %q, want %q", ct.State, "running")
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
		if time.Since(ct.CreatedAt) <= 0 {
			t.Error("CreatedAt is in the future")
		}
	})
	t.Run("with_labels", func(t *testing.T) {
		t.Parallel()
		reposData, _ := json.Marshal([]Repo{{GitRoot: "/home/user/repo", Branches: []string{"main"}}})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `,other=ignored"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 1 {
			t.Fatalf("len(Repos) = %d, want 1", len(ct.Repos))
		}
		if ct.Repos[0].GitRoot != "/home/user/repo" {
			t.Errorf("Repos[0].GitRoot = %q, want %q", ct.Repos[0].GitRoot, "/home/user/repo")
		}
		if len(ct.Repos[0].Branches) != 1 || ct.Repos[0].Branches[0] != "main" {
			t.Errorf("Repos[0].Branches = %v, want [main]", ct.Repos[0].Branches)
		}
	})
	t.Run("legacy_mounted_path_label", func(t *testing.T) {
		t.Parallel()
		reposData := []byte(`[{"git_root":"/home/user/repo","branches":["main"],"mounted_path":"/home/user/src/org/repo"}]`)
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := ct.Repos[0].ContainerPath; got != "/home/user/src/org/repo" {
			t.Errorf("ContainerPath = %q, want /home/user/src/org/repo", got)
		}
	})
	t.Run("legacy_branch_label", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runTestGit(t, t.Context(), dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, t.Context(), dir, "remote", "add", "origin", "/dev/null")
		runTestGit(t, t.Context(), dir, "remote", "add", "upstream", "/dev/null")
		runTestGit(t, t.Context(), dir, "remote", "add", "md-repo-main", "/dev/null")
		runTestGit(t, t.Context(), dir, "remote", "add", "md-other", "user@md-other:/home/user/src/"+filepath.Base(dir))
		reposData, err := json.Marshal([]map[string]string{{"git_root": dir, "branch": "main"}})
		if err != nil {
			t.Fatal(err)
		}
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer(t.Context(), &Client{Logger: testLogger(t)}, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 1 {
			t.Fatalf("len(Repos) = %d, want 1", len(ct.Repos))
		}
		if !slices.Equal(ct.Repos[0].Branches, []string{"main"}) {
			t.Fatalf("Branches = %v, want [main]", ct.Repos[0].Branches)
		}
		if want := []string{"origin", "upstream"}; !slices.Equal(ct.Repos[0].Remotes, want) {
			t.Fatalf("Remotes = %v, want %v", ct.Repos[0].Remotes, want)
		}
	})
	t.Run("no_labels", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":""}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if ct.Repos != nil {
			t.Errorf("expected nil Repos, got %v", ct.Repos)
		}
	})
	t.Run("empty_repos", func(t *testing.T) {
		t.Parallel()
		// No-repo containers encode md.repos as an empty JSON array.
		reposData, _ := json.Marshal([]Repo{})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-agent","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 0 {
			t.Errorf("expected empty Repos, got %v", ct.Repos)
		}
	})
	t.Run("podman_rfc3339", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00.123456789Z"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 123456789, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("podman_rfc3339_no_frac", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00Z"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("podman_rfc3339_offset", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00+02:00"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.FixedZone("", 2*60*60))
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("bad_created_at", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"x","State":"running","CreatedAt":"not-a-date"}`
		_, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err == nil {
			t.Fatal("expected error for bad CreatedAt")
		}
	})
	t.Run("empty_input", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalContainer(t.Context(), testClient(t), []byte(""))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})
	t.Run("bad_json", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalContainer(t.Context(), testClient(t), []byte("{not json}"))
		if err == nil {
			t.Fatal("expected error for bad JSON")
		}
	})
}

func TestAllAgentPaths(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got := allAgentPaths(nil)
		if !slices.ContainsFunc(got, func(p AgentPaths) bool {
			return slices.Contains(p.XDGConfigPaths, "md") && p.ReadOnly
		}) {
			t.Errorf("allAgentPaths(nil) = %+v, want read-only md path", got)
		}
	})

	t.Run("prepend", func(t *testing.T) {
		t.Parallel()
		input := []AgentPaths{
			{HomePaths: []string{".foo"}, XDGConfigPaths: []string{"bar"}},
			{HomePaths: []string{".baz"}, LocalSharePaths: []string{"qux"}, ReadOnly: true},
		}
		got := allAgentPaths(input)
		if len(got) != len(alwaysPaths)+len(input) {
			t.Fatalf("len(allAgentPaths(input)) = %d, want %d", len(got), len(alwaysPaths)+len(input))
		}
		if !slices.Equal(got[len(alwaysPaths)].HomePaths, []string{".foo"}) || !slices.Equal(got[len(alwaysPaths)+1].LocalSharePaths, []string{"qux"}) {
			t.Errorf("allAgentPaths(input) = %+v, want input groups preserved after alwaysPaths", got)
		}
		if !got[len(alwaysPaths)+1].ReadOnly {
			t.Errorf("allAgentPaths(input) = %+v, want input ReadOnly preserved", got)
		}
	})

	t.Run("does_not_mutate_global", func(t *testing.T) {
		t.Parallel()
		before := len(alwaysPaths)
		_ = allAgentPaths([]AgentPaths{{XDGConfigPaths: []string{"extra1", "extra2"}}})
		after := len(alwaysPaths)
		if before != after {
			t.Errorf("alwaysPaths mutated: was %d, now %d", before, after)
		}
	})
}

func TestMount(t *testing.T) {
	t.Parallel()
	t.Run("dockerArg", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		hostDir := filepath.Join(home, "data")
		if err := os.MkdirAll(hostDir, 0o750); err != nil {
			t.Fatal(err)
		}
		mount := Mount{
			HostPath:      "~/data",
			ContainerPath: "~/data",
			ReadOnly:      true,
		}
		got, err := mount.dockerArg(home)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.ToSlash(hostDir) + ":/home/user/data:ro"
		if got != want {
			t.Errorf("dockerArg = %q, want %q", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		filePath := filepath.Join(home, "file")
		if err := os.WriteFile(filePath, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name  string
			mount Mount
		}{
			{name: "missing_host", mount: Mount{HostPath: filepath.Join(home, "missing"), ContainerPath: "/data"}},
			{name: "file_host", mount: Mount{HostPath: filePath, ContainerPath: "/data"}},
			{name: "empty_container", mount: Mount{HostPath: home}},
			{name: "relative_container", mount: Mount{HostPath: home, ContainerPath: "data"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if _, err := tt.mount.dockerArg(home); err == nil {
					t.Fatal("expected error")
				}
			})
		}
	})
}

func TestParseInspectInfo(t *testing.T) {
	t.Parallel()
	t.Run("valid_docker", func(t *testing.T) {
		t.Parallel()
		cacheLabel := activeCacheSpecLabel([]activeCM{{cm: CacheMount{Name: "npm", Description: "Node", HostPath: "~/npm", ContainerPath: "/home/user/.npm", ReadOnly: true}, hostPath: "/home/me/.npm"}})
		rw := `[
			{
				"Id":"sha256:container",
				"Name":"/md-test",
				"Image":"sha256:image",
				"Architecture":"amd64",
				"Os":"linux",
				"Config":{"Image":"ghcr.io/caic/base:latest","Labels":{"md.cache_spec":"` + cacheLabel + `"}},
				"State":{"Status":"running"},
				"HostConfig":{"NanoCpus":2500000000,"CpuQuota":0,"CpuPeriod":0},
				"Mounts":[{"Source":"/host/rw","Destination":"/mnt/rw","RW":true},{"Source":"/host/ro","Destination":"/mnt/ro","RW":false}]
			}
		]`
		got, err := parseInspectInfo("docker", "md-test", []byte(rw))
		if err != nil {
			t.Fatal(err)
		}
		if got.Runtime != "docker" || got.ID != "sha256:container" || got.Name != "md-test" || got.ImageID != "sha256:image" || got.ImageRef != "ghcr.io/caic/base:latest" {
			t.Fatalf("inspect identity = %+v", got)
		}
		if got.Platform != "linux/amd64" {
			t.Errorf("Platform = %q, want linux/amd64", got.Platform)
		}
		if got.OS != "linux" || got.Architecture != "amd64" {
			t.Errorf("OS/Architecture = %q/%q, want linux/amd64", got.OS, got.Architecture)
		}
		if got.CPULimit != 3 {
			t.Errorf("CPULimit = %d, want 3", got.CPULimit)
		}
		if len(got.Mounts) != 2 || got.Mounts[0].ReadOnly || !got.Mounts[1].ReadOnly {
			t.Errorf("Mounts = %+v", got.Mounts)
		}
		if len(got.Caches) != 1 || got.Caches[0].Name != "npm" || got.Caches[0].Description != "Node" || got.Caches[0].HostPath != "/home/me/.npm" || !got.Caches[0].ReadOnly {
			t.Errorf("Caches = %+v", got.Caches)
		}
	})

	t.Run("valid_podman", func(t *testing.T) {
		t.Parallel()
		rw := `[
			{
				"Id":"ctr",
				"Image":"sha256:image2",
				"Platform":"linux/arm64",
				"Config":{"Image":"base:v2","Labels":{}},
				"State":{"Status":"exited"},
				"HostConfig":{"NanoCpus":0,"CpuQuota":150000,"CpuPeriod":100000},
				"Mounts":[{"Source":"/src","Destination":"/workspace"}]
			}
		]`
		got, err := parseInspectInfo("podman", "ctr-2", []byte(rw))
		if err != nil {
			t.Fatal(err)
		}
		if got.Platform != "linux/arm64" {
			t.Errorf("Platform = %q, want linux/arm64", got.Platform)
		}
		if got.OS != "linux" || got.Architecture != "arm64" {
			t.Errorf("OS/Architecture = %q/%q, want linux/arm64", got.OS, got.Architecture)
		}
		if got.CPULimit != 2 {
			t.Errorf("CPULimit = %d, want 2", got.CPULimit)
		}
		if got.Name != "ctr-2" {
			t.Errorf("Name = %q, want ctr-2", got.Name)
		}
	})

	t.Run("error_empty", func(t *testing.T) {
		t.Parallel()
		if _, err := parseInspectInfo("docker", "ctr", []byte(`[]`)); err == nil {
			t.Fatal("parseInspectInfo error = nil, want error")
		}
	})
}

func TestContainerInspect(t *testing.T) {
	t.Parallel()

	t.Run("valid fills missing architecture", func(t *testing.T) {
		t.Parallel()
		runtimePath, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		}
		c := &Client{
			Logger:  logger,
			Runtime: testRuntime(t, runtimePath, logger, env),
			env:     env,
		}
		info, err := (&Container{Client: c, Name: "md-test"}).Inspect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != "linux" || info.Architecture != "amd64" {
			t.Fatalf("Inspect OS/Architecture = %q/%q, want linux/amd64", info.OS, info.Architecture)
		}
	})
}

func TestFillFromInspect(t *testing.T) {
	t.Parallel()
	// Both Docker and Podman inspect return a JSON array.
	inspect := `[{
  "Name": "/md-caic-caic-3",
  "State": { "Status": "running" },
  "Created": "2025-06-15T10:30:00Z",
  "Config": {
    "Labels": {
      "md.display": "1",
      "md.tailscale": "1",
      "md.usb": "1",
      "md.sudo": "1",
      "custom": "value"
    }
  },
  "NetworkSettings": {
    "Ports": {
      "22/tcp": [{"HostPort": "32768"}],
      "5901/tcp": [{"HostPort": "32769"}]
    }
  }
}]`

	ct := &Container{Client: testClient(t)}
	if err := ct.fillFromInspect(t.Context(), []byte(inspect)); err != nil {
		t.Fatalf("fillFromInspect: %v", err)
	}
	if ct.Name != "md-caic-caic-3" {
		t.Errorf("Name = %q, want %q", ct.Name, "md-caic-caic-3")
	}
	if ct.State != "running" {
		t.Errorf("State = %q, want %q", ct.State, "running")
	}
	if !ct.Display || !ct.Tailscale || !ct.USB || !ct.Sudo {
		t.Errorf("expected all flags true, got Display=%v Tailscale=%v USB=%v Sudo=%v", ct.Display, ct.Tailscale, ct.USB, ct.Sudo)
	}
	if ct.Labels["custom"] != "value" {
		t.Errorf("custom label = %q, want value", ct.Labels["custom"])
	}
	if ct.SSHPort != 32768 || ct.VNCPort != 32769 {
		t.Errorf("ports = ssh %d vnc %d, want 32768/32769", ct.SSHPort, ct.VNCPort)
	}
	legacyDir := t.TempDir()
	runTestGit(t, t.Context(), legacyDir, "init", "-q", "--initial-branch=main")
	runTestGit(t, t.Context(), legacyDir, "remote", "add", "origin", "/dev/null")
	runTestGit(t, t.Context(), legacyDir, "remote", "add", "upstream", "/dev/null")
	runTestGit(t, t.Context(), legacyDir, "remote", "add", "md-legacy", "/dev/null")
	legacyReposData, err := json.Marshal([]map[string]string{{"git_root": legacyDir, "branch": "main"}})
	if err != nil {
		t.Fatal(err)
	}
	legacyReposB64 := base64.StdEncoding.EncodeToString(legacyReposData)
	legacyInspect := `[{"Name":"/md-legacy","State":{"Status":"running"},"Created":"2025-06-15T10:30:00Z","Config":{"Labels":{"md.repos":"` + legacyReposB64 + `"}}}]`
	ctLegacy := &Container{Client: &Client{Logger: testLogger(t)}, Logger: testLogger(t)}
	if err := ctLegacy.fillFromInspect(t.Context(), []byte(legacyInspect)); err != nil {
		t.Fatalf("legacy inspect: %v", err)
	}
	if len(ctLegacy.Repos) != 1 {
		t.Fatalf("len(legacy Repos) = %d, want 1", len(ctLegacy.Repos))
	}
	if !slices.Equal(ctLegacy.Repos[0].Branches, []string{"main"}) {
		t.Fatalf("legacy Branches = %v, want [main]", ctLegacy.Repos[0].Branches)
	}
	if want := []string{"origin", "upstream"}; !slices.Equal(ctLegacy.Repos[0].Remotes, want) {
		t.Fatalf("legacy Remotes = %v, want %v", ctLegacy.Repos[0].Remotes, want)
	}

	// Name without leading slash (Docker sometimes omits it).
	noSlash := `[{"Name":"plain","State":{"Status":"running"},"Created":"2025-06-15T10:30:00Z","Config":{"Labels":{}}}]`
	ct2 := &Container{Client: testClient(t)}
	if err := ct2.fillFromInspect(t.Context(), []byte(noSlash)); err != nil {
		t.Fatalf("no-slash name: %v", err)
	}
	if ct2.Name != "plain" {
		t.Errorf("Name = %q, want %q", ct2.Name, "plain")
	}

	// Empty array.
	if err := (&Container{Client: testClient(t)}).fillFromInspect(t.Context(), []byte(`[]`)); err == nil {
		t.Error("expected error for empty array")
	}
	// Multiple results.
	if err := (&Container{Client: testClient(t)}).fillFromInspect(t.Context(), []byte(`[{},{}]`)); err == nil {
		t.Error("expected error for multiple results")
	}
	// Bad JSON.
	if err := (&Container{Client: testClient(t)}).fillFromInspect(t.Context(), []byte(`{bad}`)); err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestUSBRunArgs(t *testing.T) {
	t.Parallel()
	got := usbRunArgs([]string{"/dev/ttyACM0", "/dev/ttyUSB0"})
	want := []string{
		"-v", "/dev/bus/usb:/dev/bus/usb",
		"--device-cgroup-rule=c 189:* rwm",
		"--device=/dev/ttyACM0",
		"--device=/dev/ttyUSB0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("USB run arguments = %v, want %v", got, want)
	}
}

func TestParseCreatedAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"docker", "2025-06-15 10:30:00 +0000 UTC", false},
		{"docker_with_tz", "2025-06-15 10:30:00 -0700 MST", false},
		{"podman_rfc3339nano", "2025-06-15T10:30:00.123456789Z", false},
		{"podman_rfc3339", "2025-06-15T10:30:00Z", false},
		{"podman_rfc3339_offset", "2025-06-15T10:30:00+02:00", false},
		{"invalid", "not-a-date", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseCreatedAt(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCreatedAt(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestTagRefspecs(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name   string
			regexp string
			tags   string
			want   []string
		}{
			{name: "empty regexp", tags: "v1\nv2"},
			{name: "selective", regexp: "^v", tags: "internal\nv1", want: []string{"refs/tags/v1:refs/tags/v1"}},
			{name: "all", regexp: ".*", tags: "internal\nv1", want: []string{"refs/tags/internal:refs/tags/internal", "refs/tags/v1:refs/tags/v1"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := tagRefspecs(tt.regexp, tt.tags)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("tagRefspecs(%q, %q) = %v, want %v", tt.regexp, tt.tags, got, tt.want)
				}
			})
		}
	})
	t.Run("large selection batches", func(t *testing.T) {
		t.Parallel()
		var tags strings.Builder
		for i := range 2000 {
			_, _ = fmt.Fprintf(&tags, "release-%04d-%s\n", i, strings.Repeat("x", 32))
		}
		tags.WriteString("internal\n")
		refspecs, err := tagRefspecs("^release-", strings.TrimSuffix(tags.String(), "\n"))
		if err != nil {
			t.Fatal(err)
		}
		batches := 0
		for start := 0; start < len(refspecs); {
			end := refspecBatchEnd(refspecs, start)
			if end <= start {
				t.Fatalf("batch did not advance from %d", start)
			}
			size := 0
			for _, refspec := range refspecs[start:end] {
				size += len(refspec) + 1
			}
			if size > maxPushRefspecBytes && end-start != 1 {
				t.Fatalf("batch size = %d, limit = %d", size, maxPushRefspecBytes)
			}
			batches++
			start = end
		}
		if batches < 2 {
			t.Fatalf("batches = %d, want multiple", batches)
		}
	})
}

func TestRepo(t *testing.T) {
	t.Parallel()
	t.Run("resolveDefaults_tracked_default_branch_remote", func(t *testing.T) {
		t.Parallel()
		for _, branch := range []string{"main", "master"} {
			t.Run(branch, func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				dir := t.TempDir()
				originDir := filepath.Join(t.TempDir(), "origin.git")
				upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
				runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch="+branch, originDir)
				runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch="+branch, upstreamDir)
				runTestGit(t, ctx, dir, "init", "-q", "--initial-branch="+branch)
				runTestGit(t, ctx, dir, "config", "user.name", "Test")
				runTestGit(t, ctx, dir, "config", "user.email", "test@test")
				writeTestFile(t, filepath.Join(dir, "README.md"), branch+"\n")
				runTestGit(t, ctx, dir, "add", ".")
				runTestGit(t, ctx, dir, "commit", "-q", "-m", branch)
				runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
				runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
				runTestGit(t, ctx, dir, "push", "-q", "origin", branch)
				runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", branch)

				repo := Repo{GitRoot: dir, Branches: []string{branch}}
				if err := repo.resolveDefaults(ctx, testLogger(t)); err != nil {
					t.Fatal(err)
				}
				if repo.DefaultRemote != "upstream" || repo.DefaultBranch != branch {
					t.Fatalf("defaults = %s/%s, want upstream/%s", repo.DefaultRemote, repo.DefaultBranch, branch)
				}
			})
		}
		t.Run("main_precedes_master", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "branch", "master")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")
			runTestGit(t, ctx, dir, "config", "branch.master.remote", "origin")
			runTestGit(t, ctx, dir, "config", "branch.master.merge", "refs/heads/master")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}}
			if err := repo.resolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "upstream" {
				t.Fatalf("default remote = %q, want upstream", repo.DefaultRemote)
			}
		})
		t.Run("explicit_remote_is_authoritative", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}, DefaultRemote: "origin"}
			if err := repo.resolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "origin" || repo.DefaultBranch != "main" {
				t.Fatalf("defaults = %s/%s, want origin/main", repo.DefaultRemote, repo.DefaultBranch)
			}
		})
		t.Run("unmapped_upstream_is_ignored", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}, Remotes: []string{"origin"}}
			if err := repo.resolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "origin" {
				t.Fatalf("default remote = %q, want origin", repo.DefaultRemote)
			}
		})
		t.Run("local_and_synthetic_upstreams_are_ignored", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			cp := "/home/user/src/repo"
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "branch", "master")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "md-task", "user@md-task:"+cp)
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/md-task/master", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", ".")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")
			runTestGit(t, ctx, dir, "config", "branch.master.remote", "md-task")
			runTestGit(t, ctx, dir, "config", "branch.master.merge", "refs/heads/master")

			repo := Repo{
				GitRoot:       dir,
				Branches:      []string{"main", "master"},
				ContainerPath: cp,
			}
			if err := repo.resolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "origin" {
				t.Fatalf("default remote = %q, want origin", repo.DefaultRemote)
			}
			if want := []string{"origin"}; !slices.Equal(repo.Remotes, want) {
				t.Fatalf("remotes = %v, want %v", repo.Remotes, want)
			}
			wantCommand := shellQuoteArgs([]string{"git", "-C", dir, "branch", "--set-upstream-to=origin/main", "master"})
			if _, err := repo.resolveContainerBranchBase(ctx, testLogger(t), "master"); err == nil || !strings.Contains(err.Error(), wantCommand) {
				t.Fatalf("resolveContainerBranchBase error = %v, want upstream repair command", err)
			}
		})
		t.Run("default_branch_comes_from_remote", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/release", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/trunk", "HEAD")
			runTestGit(t, ctx, dir, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/trunk")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/release")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}}
			if err := repo.resolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "upstream" || repo.DefaultBranch != "trunk" {
				t.Fatalf("defaults = %s/%s, want upstream/trunk", repo.DefaultRemote, repo.DefaultBranch)
			}
		})
	})
	t.Run("resolveDefaults_legacy_non_origin_remote", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "README.md"), "main\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", "main")
		cp := "/home/user/src/repo"
		runTestGit(t, ctx, dir, "remote", "add", "md-task-a", "user@md-task-a:"+cp)
		runTestGit(t, ctx, dir, "remote", "add", "md-task-b", "user@md-task-b:"+cp)

		logger := testLogger(t)
		ct := &Container{
			Logger: logger,
			Name:   "md-task-a",
			Repos:  []Repo{{GitRoot: dir, Branches: []string{"main"}, ContainerPath: cp}},
		}
		ct.migrateRepoRemotes(ctx)
		repo := &ct.Repos[0]
		if want := []string{"upstream"}; !slices.Equal(repo.Remotes, want) {
			t.Fatalf("migrated remotes = %v, want %v", repo.Remotes, want)
		}
		if err := repo.resolveDefaults(ctx, logger); err != nil {
			t.Fatal(err)
		}
		if repo.DefaultRemote != "upstream" || repo.DefaultBranch != "main" {
			t.Fatalf("defaults = %s/%s, want upstream/main", repo.DefaultRemote, repo.DefaultBranch)
		}
	})
	t.Run("migrateRepoRemotes_removed_default", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "remote", "add", "backup", "/dev/null")
		repo := Repo{
			GitRoot:       dir,
			Branches:      []string{"main"},
			ContainerPath: "/home/user/src/repo",
			DefaultRemote: "upstream",
			DefaultBranch: "main",
		}
		ct := &Container{Logger: testLogger(t), Name: "md-task", Repos: []Repo{repo}}
		ct.migrateRepoRemotes(ctx)
		if want := []string{"backup", "upstream"}; !slices.Equal(ct.Repos[0].Remotes, want) {
			t.Fatalf("migrated remotes = %v, want %v", ct.Repos[0].Remotes, want)
		}
		if err := ct.Repos[0].Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("resolveMountPaths", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				r    Repo
				want string
			}{
				{
					"basename only",
					Repo{GitRoot: "/home/user/src/myrepo"},
					"/home/user/src/myrepo",
				},
				{
					"basename with .git suffix",
					Repo{GitRoot: "/home/user/src/myrepo.git"},
					"/home/user/src/myrepo",
				},
				{
					"ContainerPath overrides basename",
					Repo{GitRoot: "/home/user/src/myrepo", ContainerPath: "/home/user/src/custom-name"},
					"/home/user/src/custom-name",
				},
				{
					"ContainerPath preserves slashes",
					Repo{GitRoot: "/home/user/src/projects/foo/website", ContainerPath: "/home/user/src/foo/website"},
					"/home/user/src/foo/website",
				},
				{
					"empty ContainerPath falls back to basename",
					Repo{GitRoot: "/home/user/src/myrepo", ContainerPath: ""},
					"/home/user/src/myrepo",
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					repos := []Repo{tt.r}
					if err := resolveContainerPaths(repos); err != nil {
						t.Fatal(err)
					}
					if got := repos[0].ContainerPath; got != tt.want {
						t.Errorf("ContainerPath = %q, want %q", got, tt.want)
					}
				})
			}
		})
	})
	t.Run("Validate", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				r    Repo
			}{
				{"from basename", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}}},
				{"explicit absolute path", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "/home/user/src/custom"}},
				{"tilde expansion", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "~/src/custom"}},
				{"bare tilde", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "~"}},
				{"multiple remotes", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", "upstream"}, DefaultRemote: "origin"}},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					if err := tt.r.Validate(); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				r    Repo
				want string
			}{
				{"empty GitRoot", Repo{}, "GitRoot is empty"},
				{"empty Branches", Repo{GitRoot: "/home/user/src/myrepo"}, "Branches is empty"},
				{"empty branch", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main", ""}}, "empty branch"},
				{"blank branch", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main", "  "}}, "with whitespace"},
				{"duplicate branch", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main", "main"}}, "duplicate branch"},
				{"empty remote", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", ""}}, "empty remote"},
				{"blank remote", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", "  "}}, "with whitespace"},
				{"duplicate remote", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", "origin"}}, "duplicate remote"},
				{"default remote missing", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"upstream"}, DefaultRemote: "origin"}, "is not in Repo.Remotes"},
				{"invalid tag regexp", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, TagRegexp: "["}, "invalid tag regexp"},
				{"relative ContainerPath", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "custom"}, "must be an absolute POSIX path"},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					err := tt.r.Validate()
					if err == nil {
						t.Fatal("expected error, got nil")
					}
					if !strings.Contains(err.Error(), tt.want) {
						t.Errorf("error = %q, want containing %q", err.Error(), tt.want)
					}
				})
			}
		})
	})
}

func TestResolveMountPaths(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name  string
			repos []Repo
			want  []string
		}{
			{
				"single repo",
				[]Repo{{GitRoot: "/home/user/src/myrepo"}},
				[]string{"/home/user/src/myrepo"},
			},
			{
				"two repos with different basenames",
				[]Repo{
					{GitRoot: "/home/user/src/foo"},
					{GitRoot: "/home/user/src/bar"},
				},
				[]string{"/home/user/src/foo", "/home/user/src/bar"},
			},
			{
				"same basename but different ContainerPath",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website", ContainerPath: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/bar/website", ContainerPath: "/home/user/src/bar/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/bar/website"},
			},
			{
				"same basename auto-disambiguated with relative path",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/bar/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/bar/website"},
			},
			{
				"mixed explicit and auto-disambiguated",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/website"},
			},
			{
				"repos outside ~/src auto-disambiguate from common parent",
				[]Repo{
					{GitRoot: "/other/foo/website"},
					{GitRoot: "/other/bar/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/bar/website"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if err := resolveContainerPaths(tt.repos); err != nil {
					t.Fatal(err)
				}
				for i, r := range tt.repos {
					if r.ContainerPath != tt.want[i] {
						t.Errorf("repos[%d].ContainerPath = %q, want %q", i, r.ContainerPath, tt.want[i])
					}
				}
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("same repo different branches still conflicts", func(t *testing.T) {
			t.Parallel()
			repos := []Repo{
				{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}},
				{GitRoot: "/home/user/src/myrepo", Branches: []string{"feature"}},
			}
			err := resolveContainerPaths(repos)
			if err == nil {
				t.Fatal("expected error for same GitRoot after relative resolution")
			}
			if !strings.Contains(err.Error(), "both mount as") {
				t.Errorf("error should mention 'both mount as', got: %v", err)
			}
		})
	})
}
