// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for git repository utilities.

package git

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

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

func TestListSubmodules(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()

	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from test code
		if d != "" {
			cmd.Dir = d
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create a bare repo to serve as the submodule remote.
	subBare := filepath.Join(dir, "sub.git")
	run("", "init", "--bare", "--initial-branch=main", subBare)
	subClone := filepath.Join(dir, "sub-clone")
	run("", "clone", subBare, subClone)
	run(subClone, "config", "user.name", "Test")
	run(subClone, "config", "user.email", "test@test")
	run(subClone, "commit", "--allow-empty", "-m", "init")
	run(subClone, "push", "origin", "main")

	// Create a main repo and add the submodule.
	main := filepath.Join(dir, "main")
	run("", "init", "--initial-branch=main", main)
	run(main, "config", "user.name", "Test")
	run(main, "config", "user.email", "test@test")
	run(main, "commit", "--allow-empty", "-m", "init")
	run(main, "-c", "protocol.file.allow=always", "submodule", "add", subBare, "lib/sub")
	run(main, "commit", "-m", "add submodule")

	subs, err := (&Checkout{Root: main, Logger: testLogger(t)}).ListSubmodules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d submodules, want 1: %v", len(subs), subs)
	}
	if subs[0].Name != "lib/sub" {
		t.Errorf("Name = %q, want %q", subs[0].Name, "lib/sub")
	}
	if subs[0].Path != "lib/sub" {
		t.Errorf("Path = %q, want %q", subs[0].Path, "lib/sub")
	}

	// Repo with no submodules returns nil.
	empty := filepath.Join(dir, "empty")
	run("", "init", "--initial-branch=main", empty)
	subs, err = (&Checkout{Root: empty, Logger: testLogger(t)}).ListSubmodules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Errorf("empty repo: got %v, want nil", subs)
	}
}

func TestFindModuleDirs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()

	// mkBare creates a minimal bare repo layout under .git/modules/<parts...>.
	mkBare := func(parts ...string) {
		t.Helper()
		base := filepath.Join(append([]string{dir, ".git", "modules"}, parts...)...)
		for _, sub := range []string{"objects", "refs"} {
			if err := os.MkdirAll(filepath.Join(base, sub), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(base, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Create the .git directory so gitRoot is detected correctly.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}

	// Direct submodule.
	mkBare("subA")
	// Nested submodule inside subA.
	mkBare("subA", "modules", "subB")
	// Another direct submodule under a path prefix.
	mkBare("lib", "subC")

	paths, err := (&Checkout{Root: dir, Logger: testLogger(t)}).FindModuleDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"lib/subC", "subA", "subA/modules/subB"}
	if !slices.Equal(paths, want) {
		t.Errorf("FindModuleDirs = %v, want %v", paths, want)
	}

	// Repo with no .git/modules returns nil.
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	paths, err = (&Checkout{Root: empty, Logger: testLogger(t)}).FindModuleDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Errorf("empty: got %v, want nil", paths)
	}
}

func TestDiscoverRepos(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create repos at various depths.
	mkGit := func(parts ...string) {
		t.Helper()
		p := append(append([]string{root}, parts...), ".git")
		if err := os.MkdirAll(filepath.Join(p...), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	mkGit("repoA")
	mkGit("org", "repoB")
	mkGit("org", "repoC")
	mkGit("deep", "nested", "repoD")
	mkGit("deep", "nested", "too", "repoE") // depth 4 — excluded at maxDepth=3

	// Hidden directory should be skipped.
	mkGit(".hidden", "repoF")

	// Nested repo inside a repo — recursion should stop at repoA.
	mkGit("repoA", "sub", ".git")

	repos, err := DiscoverCheckouts(root, 3)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(root, "deep", "nested", "repoD"),
		filepath.Join(root, "org", "repoB"),
		filepath.Join(root, "org", "repoC"),
		filepath.Join(root, "repoA"),
	}
	slices.Sort(repos)
	slices.Sort(want)

	if !slices.Equal(repos, want) {
		t.Errorf("repos = %v\n want %v", repos, want)
	}
}

func TestDiscoverReposDepthZero(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Root itself is a repo.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}

	repos, err := DiscoverCheckouts(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != root {
		t.Errorf("repos = %v, want [%s]", repos, root)
	}
}

func TestDiscoverReposEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repos, err := DiscoverCheckouts(root, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v, want empty", repos)
	}
}

func TestDiscoverReposBare(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// mkBare creates a minimal bare repo layout (HEAD file + objects/ + refs/).
	mkBare := func(parts ...string) {
		base := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Join(base, "objects"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(base, "refs"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// mkGit creates a regular (non-bare) repo layout.
	mkGit := func(parts ...string) {
		p := append(append([]string{root}, parts...), ".git")
		if err := os.MkdirAll(filepath.Join(p...), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	mkBare("myrepo.git")
	mkBare("org", "other.git")
	mkGit("regular")
	// Bare repo too deep (depth 4) — excluded at maxDepth=3.
	mkBare("deep", "nested", "too", "deep.git")

	repos, err := DiscoverCheckouts(root, 3)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(root, "myrepo.git"),
		filepath.Join(root, "org", "other.git"),
		filepath.Join(root, "regular"),
	}
	slices.Sort(repos)
	slices.Sort(want)

	if !slices.Equal(repos, want) {
		t.Errorf("repos = %v\n want %v", repos, want)
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	// Create a bare repo with "main" as the default branch, then clone it.
	type gitCmd struct {
		dir  string
		args []string
	}
	for _, c := range []gitCmd{
		{"", []string{"init", "--bare", "--initial-branch=main", bare}},
		{"", []string{"clone", bare, clone}},
		{clone, []string{"-c", "user.name=Test", "-c", "user.email=test@test", "commit", "--allow-empty", "-m", "init"}},
		{clone, []string{"push", "origin", "main"}},
	} {
		cmd := exec.CommandContext(ctx, "git", c.args...) //nolint:gosec // args are from test code
		if c.dir != "" {
			cmd.Dir = c.dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c.args, err, out)
		}
	}

	g := &Checkout{Root: clone, Logger: testLogger(t)}

	// DefaultBranch should return "main" via the symbolic ref.
	got, err := g.DefaultBranch(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("got %q, want %q", got, "main")
	}

	// Switch to a different branch and verify DefaultBranch still returns "main".
	cmd := exec.CommandContext(ctx, "git", "checkout", "-b", "feature")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b feature: %v\n%s", err, out)
	}
	got, err = g.DefaultBranch(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("got %q after checkout, want %q", got, "main")
	}

	// Remove the symbolic ref to exercise the fallback probe path.
	cmd = exec.CommandContext(ctx, "git", "remote", "set-head", "origin", "--delete")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote set-head origin --delete: %v\n%s", err, out)
	}
	got, err = g.DefaultBranch(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("got %q after deleting symbolic ref, want %q", got, "main")
	}
}

func TestPushRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	// Set up bare remote + clone with initial commit.
	type gitCmd struct {
		dir  string
		args []string
	}
	for _, c := range []gitCmd{
		{"", []string{"init", "--bare", "--initial-branch=main", bare}},
		{"", []string{"clone", bare, clone}},
		{clone, []string{"-c", "user.name=Test", "-c", "user.email=test@test", "commit", "--allow-empty", "-m", "init"}},
		{clone, []string{"push", "origin", "main"}},
		{clone, []string{"checkout", "-b", "caic-0"}},
	} {
		cmd := exec.CommandContext(ctx, "git", c.args...) //nolint:gosec // args are from test code
		if c.dir != "" {
			cmd.Dir = c.dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c.args, err, out)
		}
	}

	// Add a commit on the branch.
	if err := os.WriteFile(filepath.Join(clone, "new.txt"), []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "git", "add", ".")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, "git", "-c", "user.name=Test", "-c", "user.email=test@test", "commit", "-m", "add file")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Push the local branch ref to origin as caic-0.
	if err := (&Checkout{Root: clone, Logger: testLogger(t)}).PushRef(ctx, "caic-0", "caic-0", false); err != nil {
		t.Fatal(err)
	}

	// Verify the branch exists on the remote.
	cmd = exec.CommandContext(ctx, "git", "branch", "--list", "caic-0")
	cmd.Dir = bare
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "caic-0") {
		t.Errorf("branch caic-0 not found on remote, got: %q", string(out))
	}
}

func TestRemoteToHTTPS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"git@github.com:owner/repo", "https://github.com/owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"ssh://git@gitlab.com/owner/repo.git", "https://gitlab.com/owner/repo"},
		{"https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"http://github.com/owner/repo.git", "http://github.com/owner/repo"},
		{"", ""},
		{"  git@github.com:o/r.git  ", "https://github.com/o/r"},
	}
	for _, tt := range tests {
		got := RemoteToHTTPS(tt.in)
		if got != tt.want {
			t.Errorf("RemoteToHTTPS(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSquashOnto(t *testing.T) {
	t.Parallel()
	// Helper: set up a bare remote + clone with an initial commit on main.
	setup := func(t *testing.T) (bare, clone string) {
		t.Helper()
		ctx := t.Context()
		dir := t.TempDir()
		bare = filepath.Join(dir, "remote.git")
		clone = filepath.Join(dir, "clone")
		type gitCmd struct {
			dir  string
			args []string
		}
		for _, c := range []gitCmd{
			{"", []string{"init", "--bare", "--initial-branch=main", bare}},
			{"", []string{"clone", bare, clone}},
			{clone, []string{"config", "user.name", "Test"}},
			{clone, []string{"config", "user.email", "test@test"}},
			{clone, []string{"commit", "--allow-empty", "-m", "init"}},
			{clone, []string{"push", "origin", "main"}},
		} {
			cmd := exec.CommandContext(ctx, "git", c.args...) //nolint:gosec // args are from test code
			if c.dir != "" {
				cmd.Dir = c.dir
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", c.args, err, out)
			}
		}
		return bare, clone
	}

	// Helper: run a git command in dir.
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args are from test code
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("Basic", func(t *testing.T) {
		t.Parallel()
		bare, clone := setup(t)
		ctx := t.Context()

		// Make two commits on a feature branch.
		run(t, clone, "checkout", "-b", "feature")
		if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "add a")
		if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "add b")

		// Squash onto main.
		if err := (&Checkout{Root: clone, Logger: testLogger(t)}).SquashOnto(ctx, "feature", "main", "squash: add a + b"); err != nil {
			t.Fatal(err)
		}

		// Verify: origin/main should have exactly 2 commits (init + squash).
		log := run(t, bare, "log", "--oneline", "main")
		lines := strings.Split(log, "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 commits on main, got %d:\n%s", len(lines), log)
		}
		if !strings.Contains(lines[0], "squash: add a + b") {
			t.Errorf("expected squash commit message, got: %s", lines[0])
		}

		// Both files should be present.
		for _, name := range []string{"a.txt", "b.txt"} {
			cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", "main:"+name) //nolint:gosec // name is from test code
			cmd.Dir = bare
			if err := cmd.Run(); err != nil {
				t.Errorf("file %s missing on main after squash", name)
			}
		}
	})

	t.Run("NonFastForward", func(t *testing.T) {
		t.Parallel()
		_, clone := setup(t)
		ctx := t.Context()

		// Make a commit on a feature branch.
		run(t, clone, "checkout", "-b", "feature2")
		if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("f\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "add f")

		// Advance origin/main by pushing a new commit directly.
		run(t, clone, "checkout", "main")
		if err := os.WriteFile(filepath.Join(clone, "other.txt"), []byte("other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "advance main")
		run(t, clone, "push", "origin", "main")

		// SquashOnto will fetch (getting latest main), create a squash commit
		// parented on the fetched main, and push. This should succeed because
		// origin/main was refreshed by the Fetch inside SquashOnto.
		if err := (&Checkout{Root: clone, Logger: testLogger(t)}).SquashOnto(ctx, "feature2", "main", "squash f"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIsReachable(t *testing.T) { //nolint:tparallel // subtests share git repo
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	run := func(t *testing.T, d string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from test code
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Set up bare remote + clone with initial commit on main.
	run(t, "", "init", "--bare", "--initial-branch=main", bare)
	run(t, "", "clone", bare, clone)
	run(t, clone, "config", "user.name", "Test")
	run(t, clone, "config", "user.email", "test@test")
	run(t, clone, "commit", "--allow-empty", "-m", "init")
	run(t, clone, "push", "origin", "main")

	// Add a container remote with a new commit unreachable from origin.
	containerBare := filepath.Join(dir, "container.git")
	run(t, "", "init", "--bare", "--initial-branch=main", containerBare)
	run(t, clone, "remote", "add", "md-caic-w0", containerBare)

	// Create a commit on a feature branch and push only to the container remote.
	run(t, clone, "checkout", "-b", "caic/w0")
	if err := os.WriteFile(filepath.Join(clone, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-m", "container work")
	containerCommit := run(t, clone, "rev-parse", "HEAD")
	run(t, clone, "push", "md-caic-w0", "caic/w0")
	run(t, clone, "checkout", "main")
	run(t, clone, "branch", "-D", "caic/w0")

	// The initial commit is on origin/main — reachable.
	initCommit := run(t, clone, "rev-parse", "origin/main")

	g := &Checkout{Root: clone, Logger: testLogger(t)}

	t.Run("Reachable", func(t *testing.T) { //nolint:paralleltest // subtests share git repo
		ok, err := g.IsReachable(ctx, initCommit)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("expected commit on origin/main to be reachable")
		}
	})

	t.Run("Unreachable", func(t *testing.T) { //nolint:paralleltest // subtests share git repo
		ok, err := g.IsReachable(ctx, containerCommit)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected commit only on container remote to be unreachable")
		}
	})

	t.Run("ReachableViaLocalBranch", func(t *testing.T) { //nolint:paralleltest // subtests share git repo
		// Create a local branch pointing at the container commit.
		run(t, clone, "branch", "local-backup", containerCommit)
		defer func() {
			run(t, clone, "branch", "-D", "local-backup")
		}()
		ok, err := g.IsReachable(ctx, containerCommit)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("expected commit on local branch to be reachable")
		}
	})
}

func TestCreateBranch(t *testing.T) { //nolint:tparallel // subtests share git repo
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()

	run := func(t *testing.T, d string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from test code
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run(t, "", "init", "--initial-branch=main", dir)
	run(t, dir, "config", "user.name", "Test")
	run(t, dir, "config", "user.email", "test@test")
	run(t, dir, "commit", "--allow-empty", "-m", "init")
	startCommit := run(t, dir, "rev-parse", "HEAD")

	g := &Checkout{Root: dir, Logger: testLogger(t)}

	t.Run("DoesNotChangeWorkingTree", func(t *testing.T) { //nolint:paralleltest // subtests share git repo
		if err := g.CreateBranch(ctx, "caic-1", "main"); err != nil {
			t.Fatal(err)
		}
		// Branch points at the same commit as main.
		got := run(t, dir, "rev-parse", "caic-1")
		if got != startCommit {
			t.Errorf("branch points at %s, want %s", got, startCommit)
		}
		// Working tree must still be on main, not caic-1.
		current := run(t, dir, "branch", "--show-current")
		if current != "main" {
			t.Errorf("current branch is %q, want %q", current, "main")
		}
	})

	t.Run("AlreadyExists", func(t *testing.T) { //nolint:paralleltest // subtests share git repo
		if err := g.CreateBranch(ctx, "caic-1", "main"); err == nil {
			t.Error("expected error for duplicate branch")
		}
	})
}
