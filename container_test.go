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
}

func TestContainer(t *testing.T) { //nolint:tparallel // Pull uses t.Setenv for fake ssh.
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
				Client: &Client{},
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branch:        "caic-1",
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
				Client: &Client{},
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branch:        "main",
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
				Client: &Client{},
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branch:        "feature",
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

		repo := Repo{GitRoot: dir, Branch: "main"}
		if err := repo.resolveDefaults(ctx); err != nil {
			t.Fatal(err)
		}
		base, err := repo.resolveContainerBranchBase(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != "upstream/main" || base.useHost || base.destination != "refs/remotes/upstream/main" {
			t.Fatalf("remote base = %+v, want upstream/main without host", base)
		}

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local main\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "local main")
		base, err = repo.resolveContainerBranchBase(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != "host/main" || !base.useHost || base.destination != "refs/remotes/host/main" {
			t.Fatalf("local base = %+v, want host/main", base)
		}
	})
	t.Run("Pull", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		t.Run("updates_container_diff_base", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
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
				Client: &Client{Home: home, Runtime: "true"},
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branch:        "main",
					MountedPath:   containerPath,
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, 0, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
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

func TestUnmarshalContainer(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC"}`
		ct, err := unmarshalContainer([]byte(raw))
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
		reposData, _ := json.Marshal([]Repo{{GitRoot: "/home/user/repo", Branch: "main"}})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `,other=ignored"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 1 {
			t.Fatalf("len(Repos) = %d, want 1", len(ct.Repos))
		}
		if ct.Repos[0].GitRoot != "/home/user/repo" {
			t.Errorf("Repos[0].GitRoot = %q, want %q", ct.Repos[0].GitRoot, "/home/user/repo")
		}
		if ct.Repos[0].Branch != "main" {
			t.Errorf("Repos[0].Branch = %q, want %q", ct.Repos[0].Branch, "main")
		}
	})
	t.Run("no_labels", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":""}`
		ct, err := unmarshalContainer([]byte(raw))
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
		ct, err := unmarshalContainer([]byte(raw))
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
		ct, err := unmarshalContainer([]byte(raw))
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
		ct, err := unmarshalContainer([]byte(raw))
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
		ct, err := unmarshalContainer([]byte(raw))
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
		_, err := unmarshalContainer([]byte(raw))
		if err == nil {
			t.Fatal("expected error for bad CreatedAt")
		}
	})
	t.Run("empty_input", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalContainer([]byte(""))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})
	t.Run("bad_json", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalContainer([]byte("{not json}"))
		if err == nil {
			t.Fatal("expected error for bad JSON")
		}
	})
}

func TestParseStatsLine(t *testing.T) {
	t.Parallel()
	t.Run("normal", func(t *testing.T) {
		t.Parallel()
		line := `{"Name":"md-repo-main","CPUPerc":"1.23%","MemUsage":"150MiB / 7.5GiB","MemPerc":"1.95%","PIDs":"12","NetIO":"1.5kB / 500B","BlockIO":"10MB / 2MB"}`
		s, name, err := parseStatsLine(line)
		if err != nil {
			t.Fatal(err)
		}
		if name != "md-repo-main" {
			t.Errorf("name = %q, want %q", name, "md-repo-main")
		}
		if s.CPUPerc != 1.23 {
			t.Errorf("CPUPerc = %v, want 1.23", s.CPUPerc)
		}
		if s.MemUsed != 150<<20 {
			t.Errorf("MemUsed = %d, want %d", s.MemUsed, 150<<20)
		}
		if s.PIDs != 12 {
			t.Errorf("PIDs = %d, want 12", s.PIDs)
		}
	})
	t.Run("na_values", func(t *testing.T) {
		t.Parallel()
		// docker stats returns N/A when cgroup metrics are unavailable (e.g. DinD).
		line := `{"Name":"md-repo-main","CPUPerc":"N/A","MemUsage":"N/A / N/A","MemPerc":"N/A","PIDs":"N/A","NetIO":"N/A / N/A","BlockIO":"N/A / N/A"}`
		s, name, err := parseStatsLine(line)
		if err != nil {
			t.Fatalf("N/A values should not cause an error, got: %v", err)
		}
		if name != "md-repo-main" {
			t.Errorf("name = %q, want %q", name, "md-repo-main")
		}
		if s.CPUPerc != 0 || s.MemUsed != 0 || s.MemLimit != 0 || s.NetRx != 0 || s.NetTx != 0 {
			t.Errorf("expected all-zero stats for N/A, got %+v", s)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"empty", ""},
			{"bad_json", "{invalid json}"},
			{"bad_cpu", `{"Name":"x","CPUPerc":"bad%","MemUsage":"0B / 0B","MemPerc":"0%","PIDs":"0","NetIO":"0B / 0B","BlockIO":"0B / 0B"}`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := parseStatsLine(tt.in)
				if err == nil {
					t.Errorf("parseStatsLine(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
			want uint64
		}{
			{"zero_bytes", "0B", 0},
			{"bytes", "100B", 100},
			{"kib", "1KiB", 1 << 10},
			{"mib", "150MiB", 150 << 20},
			{"gib", "7.5GiB", uint64(7.5 * float64(1<<30))},
			{"tib", "1TiB", 1 << 40},
			{"kb", "1.5kB", 1500},
			{"mb", "10MB", 10_000_000},
			{"gb", "1GB", 1_000_000_000},
			{"tb", "2TB", 2_000_000_000_000},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := parseByteSize(tt.in)
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Errorf("parseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
				}
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"unknown_unit", "100XB"},
			{"no_unit", "100"},
			{"empty", ""},
			{"bad_number", "abcMiB"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, err := parseByteSize(tt.in)
				if err == nil {
					t.Errorf("parseByteSize(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseMemUsage(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		used, limit, err := parseMemUsage("150MiB / 7.5GiB")
		if err != nil {
			t.Fatal(err)
		}
		if used != 150<<20 {
			t.Errorf("used = %d, want %d", used, 150<<20)
		}
		if limit != uint64(7.5*float64(1<<30)) {
			t.Errorf("limit = %d, want %d", limit, uint64(7.5*float64(1<<30)))
		}
	})

	t.Run("na", func(t *testing.T) {
		t.Parallel()
		used, limit, err := parseMemUsage("N/A / N/A")
		if err != nil {
			t.Fatal(err)
		}
		if used != 0 || limit != 0 {
			t.Errorf("expected (0, 0), got (%d, %d)", used, limit)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"no_slash", "150MiB"},
			{"bad_used", "abc / 1GiB"},
			{"bad_limit", "1MiB / xyz"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := parseMemUsage(tt.in)
				if err == nil {
					t.Errorf("parseMemUsage(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseIOPair(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		a, b, err := parseIOPair("1.5kB / 500B")
		if err != nil {
			t.Fatal(err)
		}
		if a != 1500 {
			t.Errorf("a = %d, want 1500", a)
		}
		if b != 500 {
			t.Errorf("b = %d, want 500", b)
		}
	})

	t.Run("na", func(t *testing.T) {
		t.Parallel()
		a, b, err := parseIOPair("N/A / N/A")
		if err != nil {
			t.Fatal(err)
		}
		if a != 0 || b != 0 {
			t.Errorf("expected (0, 0), got (%d, %d)", a, b)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"no_slash", "100kB"},
			{"bad_first", "abc / 1kB"},
			{"bad_second", "1kB / xyz"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := parseIOPair(tt.in)
				if err == nil {
					t.Errorf("parseIOPair(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestMergePaths(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got := mergePaths(nil)
		// Should return alwaysPaths base.
		if len(got.XDGConfigPaths) < 2 {
			t.Errorf("expected at least 2 XDGConfigPaths from alwaysPaths, got %d", len(got.XDGConfigPaths))
		}
	})

	t.Run("merge", func(t *testing.T) {
		t.Parallel()
		input := []AgentPaths{
			{HomePaths: []string{".foo"}, XDGConfigPaths: []string{"bar"}},
			{HomePaths: []string{".baz"}, LocalSharePaths: []string{"qux"}},
		}
		got := mergePaths(input)
		if !slices.Contains(got.HomePaths, ".foo") || !slices.Contains(got.HomePaths, ".baz") {
			t.Errorf("HomePaths = %v, want .foo and .baz", got.HomePaths)
		}
		if !slices.Contains(got.XDGConfigPaths, "bar") {
			t.Errorf("XDGConfigPaths = %v, want bar", got.XDGConfigPaths)
		}
		if !slices.Contains(got.LocalSharePaths, "qux") {
			t.Errorf("LocalSharePaths = %v, want qux", got.LocalSharePaths)
		}
	})

	t.Run("does_not_mutate_global", func(t *testing.T) {
		t.Parallel()
		before := len(alwaysPaths.XDGConfigPaths)
		_ = mergePaths([]AgentPaths{{XDGConfigPaths: []string{"extra1", "extra2"}}})
		after := len(alwaysPaths.XDGConfigPaths)
		if before != after {
			t.Errorf("alwaysPaths.XDGConfigPaths mutated: was %d, now %d", before, after)
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
		c := &Client{
			Runtime: runtimePath,
			env: []string{
				fakeRuntimeEnv + "=1",
				fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			},
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

	ct := &Container{}
	if err := fillFromInspect(ct, []byte(inspect)); err != nil {
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

	// Name without leading slash (Docker sometimes omits it).
	noSlash := `[{"Name":"plain","State":{"Status":"running"},"Created":"2025-06-15T10:30:00Z","Config":{"Labels":{}}}]`
	ct2 := &Container{}
	if err := fillFromInspect(ct2, []byte(noSlash)); err != nil {
		t.Fatalf("no-slash name: %v", err)
	}
	if ct2.Name != "plain" {
		t.Errorf("Name = %q, want %q", ct2.Name, "plain")
	}

	// Empty array.
	if err := fillFromInspect(&Container{}, []byte(`[]`)); err == nil {
		t.Error("expected error for empty array")
	}
	// Multiple results.
	if err := fillFromInspect(&Container{}, []byte(`[{},{}]`)); err == nil {
		t.Error("expected error for multiple results")
	}
	// Bad JSON.
	if err := fillFromInspect(&Container{}, []byte(`{bad}`)); err == nil {
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
				{"from basename", Repo{GitRoot: "/home/user/src/myrepo"}},
				{"explicit absolute path", Repo{GitRoot: "/home/user/src/myrepo", MountedPath: "/home/user/src/custom"}},
				{"tilde expansion", Repo{GitRoot: "/home/user/src/myrepo", MountedPath: "~/src/custom"}},
				{"bare tilde", Repo{GitRoot: "/home/user/src/myrepo", MountedPath: "~"}},
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
				{"relative MountedPath", Repo{GitRoot: "/home/user/src/myrepo", MountedPath: "custom"}, "must be an absolute POSIX path"},
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
				{GitRoot: "/home/user/src/myrepo", Branch: "main"},
				{GitRoot: "/home/user/src/myrepo", Branch: "feature"},
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
