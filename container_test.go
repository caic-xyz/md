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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func runTestGit(t *testing.T, ctx context.Context, wd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from test code
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "LANG=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
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

		diffCommand := gitDiffCommand(dir, nil, false)
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

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(dir, []string{"--name-only"}, false)) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "local.txt") {
			t.Errorf("diff output missing local.txt:\n%s", out)
		}
		if strings.Contains(out, "upstream.txt") {
			t.Errorf("diff output includes updated upstream file:\n%s", out)
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

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(dir, nil, false)) //nolint:gosec // repo path is a test temp dir
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

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(dir, []string{"--name-only"}, false)) //nolint:gosec // repo path is a test temp dir
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
}

func TestContainer(t *testing.T) { //nolint:tparallel // Pull uses fakeSSH with t.Setenv.
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
		t.Run("missing_recorded_default_branch", func(t *testing.T) {
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
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatalf("SyncDefaultBranch with deleted recorded default branch: %v", err)
			}
		})
	})
	t.Run("resolveContainerBranchBase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		upstreamDir := filepath.Join(t.TempDir(), "upstream.git")

		runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", "main")

		repo := Repo{GitRoot: dir, Branches: []string{"main"}}
		logger := testLogger(t)
		if err := repo.resolveDefaults(ctx, logger); err != nil {
			t.Fatal(err)
		}
		base, err := repo.resolveContainerBranchBase(ctx, logger, "main")
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != "upstream/main" || base.useHost || base.destination != "refs/remotes/upstream/main" {
			t.Fatalf("remote base = %+v, want upstream/main without host", base)
		}

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local main\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "local main")
		base, err = repo.resolveContainerBranchBase(ctx, logger, "main")
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != "host/main" || !base.useHost || base.destination != "refs/remotes/host/main" {
			t.Fatalf("local base = %+v, want host/main", base)
		}
	})
	t.Run("Pull", func(t *testing.T) {
		t.Run("updates_container_diff_base", func(t *testing.T) {
			ctx := t.Context()
			fakeSSH(t)
			sshLogPath := filepath.Join(t.TempDir(), "ssh.log")
			t.Setenv(fakeSSHLogEnv, sshLogPath)
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
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRuntime(t, "true", testLogger(t), nil)},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main"},
					MountedPath:   containerPath,
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			sshLog, err := os.ReadFile(sshLogPath) //nolint:gosec // test log is under t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			sshCommands := strings.FieldsFunc(string(sshLog), func(r rune) bool { return r == '\n' })
			if len(sshCommands) > 2 {
				t.Fatalf("ssh invocations = %d, want at most 2\n%s", len(sshCommands), sshLog)
			}
			hasCombinedUpdate := false
			for _, cmd := range sshCommands {
				if strings.Contains(cmd, "git config --replace-all remote.origin.url") && strings.Contains(cmd, "git switch -q -C main") {
					hasCombinedUpdate = true
					break
				}
			}
			if !hasCombinedUpdate {
				t.Fatalf("missing combined remote config and branch update ssh command:\n%s", sshLog)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); got != "host/main" {
				t.Fatalf("container upstream = %q, want host/main", got)
			}

			cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(containerPath, nil, false)) //nolint:gosec // repo path is a test temp dir
			cmd.Env = append(os.Environ(), "LANG=C")
			stdout.Reset()
			stderr.Reset()
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if out := stdout.String(); out != "" {
				t.Fatalf("diff after pull = %q, want empty", out)
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
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRuntime(t, "true", testLogger(t), nil)},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main"},
					MountedPath:   filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Fetch(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Fetch: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if got := runTestGit(t, ctx, hostDir, "show", "md-test/main:container.txt"); got != "container" {
				t.Fatalf("fetched container.txt = %q, want container", got)
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

			logger := testLogger(t)
			ct := &Container{
				Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, "true", logger, nil)},
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main", "feature"},
					MountedPath:   filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
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
		t.Run("deleted_source_branch", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
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
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRuntime(t, "true", testLogger(t), nil)},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{branch},
					MountedPath:   filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: branch,
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Diff(ctx, &stdout, &stderr, 0, []string{"--name-only"}); err != nil {
				t.Fatalf("Diff: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != "container.txt" {
				t.Fatalf("diff --name-only = %q, want container.txt", got)
			}

			stdout.Reset()
			stderr.Reset()
			if err := ct.Pull(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if got := runTestGit(t, ctx, hostDir, "show", branch+":container.txt"); got != "container" {
				t.Fatalf("pulled container.txt = %q, want container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "main" {
				t.Fatalf("current branch = %q, want main", got)
			}
		})
	})
	t.Run("Push", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
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
				Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, "true", logger, nil)},
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main", "feature"},
					MountedPath:   containerPath,
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
		t.Run("branch_names_fork_primary_branch_only", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			gitRoot := t.TempDir()
			runTestGit(t, ctx, gitRoot, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, gitRoot, "config", "user.name", "Test")
			runTestGit(t, ctx, gitRoot, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(gitRoot, "README.md"), "main\n")
			runTestGit(t, ctx, gitRoot, "add", ".")
			runTestGit(t, ctx, gitRoot, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, gitRoot, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(gitRoot, "README.md"), "feature\n")
			runTestGit(t, ctx, gitRoot, "commit", "-q", "-am", "feature")

			existing := []*Container{{Repos: []Repo{{GitRoot: gitRoot, Branches: []string{"other", "feature-0"}}}}}
			repo := &Repo{GitRoot: gitRoot, Branches: []string{"main", "feature"}}
			got, err := forkRepoBranches(ctx, repo, existing)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"main-0", "feature"}
			if !slices.Equal(got, want) {
				t.Fatalf("forkRepoBranches = %v, want %v", got, want)
			}
		})
		t.Run("error_git_check", func(t *testing.T) {
			t.Parallel()
			_, err := forkRepoBranches(t.Context(), &Repo{GitRoot: filepath.Join(t.TempDir(), "missing"), Branches: []string{"main"}}, nil)
			if err == nil {
				t.Fatal("forkRepoBranches error = nil, want git check error")
			}
		})
	})
	t.Run("Signal", func(t *testing.T) {
		t.Run("error invalid pid", func(t *testing.T) {
			t.Parallel()
			ct := &Container{Name: "md-test"}
			if err := ct.Signal(t.Context(), 0, "SIGTERM"); err == nil {
				t.Fatal("Signal error = nil, want invalid pid error")
			}
		})
	})
}

func TestFork(t *testing.T) {
	t.Parallel()

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
	t.Run("legacy_branch_label", func(t *testing.T) {
		t.Parallel()
		reposData := []byte(`[{"git_root":"/home/user/repo","branch":"main"}]`)
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
	legacyReposData := []byte(`[{"git_root":"/home/user/repo","branch":"main"}]`)
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

func TestParsePSOutput(t *testing.T) {
	t.Parallel()
	out := `    1     0 root     Ss    0.0  0.1 00:00:01 /sbin/init
  123     1 user     Sl    1.5  2.5 00:00:03 agent run --flag value
  124   123 user     R     0.0  0.0 00:00:00 ps -eo pid,ppid,user,stat,%cpu,%mem,time,args --no-headers
broken
`
	procs, err := parsePSOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 {
		t.Fatalf("processes = %+v, want 2 entries after filtering ps", procs)
	}
	if procs[1].PID != 123 || procs[1].PPID != 1 || procs[1].User != "user" || procs[1].CPU != 1.5 || procs[1].Mem != 2.5 || procs[1].Command != "agent run --flag value" {
		t.Errorf("process = %+v, want parsed agent command", procs[1])
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

func TestRepo(t *testing.T) {
	t.Parallel()
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
					"MountedPath overrides basename",
					Repo{GitRoot: "/home/user/src/myrepo", MountedPath: "/home/user/src/custom-name"},
					"/home/user/src/custom-name",
				},
				{
					"MountedPath preserves slashes",
					Repo{GitRoot: "/home/user/src/projects/foo/website", MountedPath: "/home/user/src/foo/website"},
					"/home/user/src/foo/website",
				},
				{
					"empty MountedPath falls back to basename",
					Repo{GitRoot: "/home/user/src/myrepo", MountedPath: ""},
					"/home/user/src/myrepo",
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					repos := []Repo{tt.r}
					if err := resolveMountPaths(repos); err != nil {
						t.Fatal(err)
					}
					if got := repos[0].MountedPath; got != tt.want {
						t.Errorf("MountedPath = %q, want %q", got, tt.want)
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
				{"explicit absolute path", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, MountedPath: "/home/user/src/custom"}},
				{"tilde expansion", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, MountedPath: "~/src/custom"}},
				{"bare tilde", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, MountedPath: "~"}},
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
				{"relative MountedPath", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, MountedPath: "custom"}, "must be an absolute POSIX path"},
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
				"same basename but different MountedPath",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website", MountedPath: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/bar/website", MountedPath: "/home/user/src/bar/website"},
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
				if err := resolveMountPaths(tt.repos); err != nil {
					t.Fatal(err)
				}
				for i, r := range tt.repos {
					if r.MountedPath != tt.want[i] {
						t.Errorf("repos[%d].MountedPath = %q, want %q", i, r.MountedPath, tt.want[i])
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
			err := resolveMountPaths(repos)
			if err == nil {
				t.Fatal("expected error for same GitRoot after relative resolution")
			}
			if !strings.Contains(err.Error(), "both mount as") {
				t.Errorf("error should mention 'both mount as', got: %v", err)
			}
		})
	})
}
