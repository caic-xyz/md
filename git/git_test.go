// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for git repository utilities.

package git

import (
	"bytes"
	"crypto/rand"
	"errors"
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

func canonicalTempDir(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiscoverCheckouts(t *testing.T) {
	t.Parallel()
	t.Run("Nested", func(t *testing.T) {
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
	})

	t.Run("DepthZero", func(t *testing.T) {
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
	})

	t.Run("Empty", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		repos, err := DiscoverCheckouts(root, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(repos) != 0 {
			t.Errorf("repos = %v, want empty", repos)
		}
	})

	t.Run("Bare", func(t *testing.T) {
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
	})
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

func TestObjectStore(t *testing.T) {
	ctx := t.Context()
	root := filepath.Join(canonicalTempDir(t), "checkout")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(canonicalTempDir(t), "retained metadata")
	c := &Checkout{Root: root}
	run := func(args ...string) string {
		out, err := c.RunGit(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("init", "-q", "--initial-branch=main", "--separate-git-dir="+filepath.ToSlash(dir))
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("stored content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-qm", "initial")
	want := run("rev-parse", "HEAD")
	run("remote", "add", "upstream", "https://example.invalid/test.git")
	run("update-ref", "refs/remotes/upstream/trunk", want)
	run("symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/trunk")
	relativeRoot, err := filepath.Rel(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	run("config", "core.worktree", filepath.ToSlash(relativeRoot))
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	store := &ObjectStore{GitDir: dir}
	t.Setenv("GIT_WORK_TREE", filepath.ToSlash(dir))
	t.Run("RevParse", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
		if got, err := store.RevParse(t.Context(), "HEAD"); err != nil || got != want {
			t.Fatalf("retained HEAD = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("DefaultRemote", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
		if got, err := store.DefaultRemote(t.Context()); err != nil || got != "upstream" {
			t.Fatalf("default remote = %q, %v; want upstream", got, err)
		}
	})
	t.Run("DefaultBranch", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
		if got, err := store.DefaultBranch(t.Context(), "upstream"); err != nil || got != "trunk" {
			t.Fatalf("default branch = %q, %v; want trunk", got, err)
		}
	})
	t.Run("Command", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
		var content strings.Builder
		cmd := store.Command(t.Context(), "show", "HEAD:tracked.txt")
		cmd.Stdout = &content
		if err := cmd.Run(); err != nil || content.String() != "stored content\n" {
			t.Fatalf("streamed object = %q, %v", content.String(), err)
		}
	})
	t.Run("RunGit", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
		t.Run("valid", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
			configPath := filepath.Join(dir, "config")
			before, err := os.ReadFile(configPath) //nolint:gosec // Test-owned Git configuration.
			if err != nil {
				t.Fatal(err)
			}
			wt := filepath.Join(canonicalTempDir(t), "linked")
			if _, err := store.RunGit(t.Context(), "worktree", "add", "--detach", filepath.ToSlash(wt), want); err != nil {
				t.Fatal(err)
			}
			if content, err := os.ReadFile(filepath.Join(wt, "tracked.txt")); err != nil || string(content) != "stored content\n" { //nolint:gosec // Test-owned temp worktree.
				t.Fatalf("linked worktree = %q, %v", content, err)
			}
			checkout := &Checkout{Root: wt, Logger: testLogger(t)}
			adminDir, err := checkout.RunGit(t.Context(), "rev-parse", "--absolute-git-dir")
			if err != nil {
				t.Fatal(err)
			}
			linkedStore := &ObjectStore{GitDir: adminDir}
			if got, err := linkedStore.RevParse(t.Context(), "refs/remotes/upstream/trunk"); err != nil || got != want {
				t.Fatalf("linked store shared ref = %q, %v; want %q", got, err, want)
			}
			if _, err := store.RunGit(t.Context(), "worktree", "remove", filepath.ToSlash(wt)); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(configPath) //nolint:gosec // Test-owned Git configuration.
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("metadata scope changed the repository config: %v", err)
			}
		})
		t.Run("error", func(t *testing.T) { //nolint:paralleltest // The test sets GIT_WORK_TREE.
			if _, err := store.RunGit(t.Context(), "checkout", "--", "tracked.txt"); err == nil || !strings.Contains(err.Error(), "must be run in a work tree") {
				t.Fatalf("checkout in object store = %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "tracked.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("checked out object in metadata: %v", err)
			}
			rel := &ObjectStore{GitDir: "relative.git"}
			if _, err := rel.RunGit(t.Context(), "status"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
				t.Fatalf("relative Git directory = %v", err)
			}
		})
	})
}

func TestCheckout(t *testing.T) {
	t.Parallel()
	t.Run("Command", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		root := canonicalTempDir(t)
		g := &Checkout{Root: root, Logger: testLogger(t)}
		run := func(c *Checkout, args ...string) string {
			out, err := c.RunGit(ctx, args...)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		run(g, "init", "-q", "--initial-branch=main")
		run(g, "config", "user.name", "Test")
		run(g, "config", "user.email", "test@example.invalid")
		if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("stored content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(g, "add", ".")
		run(g, "commit", "-qm", "initial")
		tip := run(g, "rev-parse", "HEAD")
		if got := run(g, "rev-parse", "--show-toplevel"); got != filepath.ToSlash(root) {
			t.Fatalf("ordinary checkout root = %q", got)
		}
		parent := &Checkout{Root: canonicalTempDir(t), Logger: testLogger(t)}
		run(parent, "init", "-q", "--initial-branch=main")
		run(parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", root, "sub")
		scopedParent := &Checkout{Root: parent.Root, GitDir: filepath.Join(parent.Root, ".git"), Logger: testLogger(t)}
		modules, err := scopedParent.ListSubmodules(ctx)
		if err != nil || len(modules) != 1 || modules[0].Path != "sub" {
			t.Fatalf("scoped checkout modules = %+v, %v", modules, err)
		}
		moduleRoot := filepath.Join(parent.Root, "sub")
		module := &Checkout{Root: moduleRoot, Logger: testLogger(t)}
		moduleDir := run(module, "rev-parse", "--absolute-git-dir")
		module.GitDir = moduleDir
		if err := module.CreateBranch(ctx, "scoped", "HEAD", false); err != nil {
			t.Fatal(err)
		}
		if err := module.CheckoutBranch(ctx, "scoped"); err != nil {
			t.Fatal(err)
		}
		if got, err := module.CurrentBranch(ctx); err != nil || got != "scoped" {
			t.Fatalf("scoped checkout branch = %q, %v", got, err)
		}
		roots, err := module.Superprojects(ctx)
		if err != nil || !slices.Equal(roots, []string{filepath.ToSlash(parent.Root)}) {
			t.Fatalf("scoped superprojects = %v, %v", roots, err)
		}
		if err := os.RemoveAll(moduleRoot); err != nil {
			t.Fatal(err)
		}
		bare := filepath.Join(canonicalTempDir(t), "bare.git")
		run(g, "clone", "-q", "--bare", root, bare)
		bareCheckout := &Checkout{Root: bare, GitDir: bare, Logger: testLogger(t)}
		run(bareCheckout, "update-ref", "refs/remotes/origin/main", tip)
		run(bareCheckout, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		for _, tc := range []struct{ name, root, gitDir string }{
			{"removed_module", moduleRoot, moduleDir},
			{"object_only_module", "", moduleDir},
			{"bare", "", bare},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				checkout := &Checkout{Root: tc.root, GitDir: tc.gitDir, Logger: testLogger(t)}
				if _, err := checkout.ListSubmodules(t.Context()); err == nil || !strings.Contains(err.Error(), "worktree") {
					t.Fatalf("object-only ListSubmodules = %v", err)
				}
				if _, err := checkout.Superprojects(t.Context()); err == nil || !strings.Contains(err.Error(), "worktree") {
					t.Fatalf("object-only Superprojects = %v", err)
				}
				if err := checkout.CheckoutBranch(t.Context(), "main"); err == nil || !strings.Contains(err.Error(), "worktree") {
					t.Fatalf("object-only CheckoutBranch = %v", err)
				}
				if got, err := checkout.RevParse(t.Context(), "HEAD"); err != nil || got != tip {
					t.Fatalf("retained HEAD = %q, %v", got, err)
				}
				var content strings.Builder
				cmd := checkout.Command(t.Context(), "show", "HEAD:tracked.txt")
				cmd.Stdout = &content
				if err := cmd.Run(); err != nil || content.String() != "stored content\n" {
					t.Fatalf("streamed object = %q, %v", content.String(), err)
				}
				if got, err := checkout.DefaultRemote(t.Context()); err != nil || got != "origin" {
					t.Fatalf("default remote = %q, %v", got, err)
				}
				if got, err := checkout.DefaultBranch(t.Context(), "origin"); err != nil || got != "main" {
					t.Fatalf("default branch = %q, %v", got, err)
				}
				before, err := os.ReadFile(filepath.Join(tc.gitDir, "config"))
				if err != nil {
					t.Fatal(err)
				}
				worktree := filepath.Join(canonicalTempDir(t), "review")
				if _, err := checkout.RunGit(t.Context(), "worktree", "add", "--detach", worktree, tip); err != nil {
					t.Fatal(err)
				}
				if _, err := checkout.RunGit(t.Context(), "worktree", "remove", worktree); err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(filepath.Join(tc.gitDir, "config"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("explicit metadata scope changed repository config")
				}
			})
		}
	})
	t.Run("ListSubmodules", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := canonicalTempDir(t)

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

		sub := filepath.Join(main, "lib", "sub")
		run(sub, "-c", "protocol.file.allow=always", "submodule", "add", subBare, "nested")
		roots, err := (&Checkout{Root: filepath.Join(sub, "nested"), Logger: testLogger(t)}).Superprojects(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(roots, []string{filepath.ToSlash(sub), filepath.ToSlash(main)}) {
			t.Fatalf("nested superprojects = %v", roots)
		}
		roots, err = (&Checkout{Root: main, Logger: testLogger(t)}).Superprojects(ctx)
		if err != nil || len(roots) != 0 {
			t.Fatalf("ordinary checkout superprojects = %v, %v", roots, err)
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
	})

	t.Run("FindModuleDirs", func(t *testing.T) {
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
	})

	t.Run("DefaultBranch", func(t *testing.T) {
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
	})

	t.Run("PushRef", func(t *testing.T) {
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

		// Select a named remote even when origin points elsewhere.
		g := &Checkout{Root: clone, Logger: testLogger(t)}
		if _, err := g.RunGit(ctx, "remote", "rename", "origin", "destination"); err != nil {
			t.Fatal(err)
		}
		if _, err := g.RunGit(ctx, "remote", "add", "origin", filepath.Join(dir, "missing.git")); err != nil {
			t.Fatal(err)
		}
		// Push the local branch ref to destination as caic-0.
		if err := g.PushRef(ctx, "destination", "caic-0", "caic-0", false); err != nil {
			t.Fatal(err)
		}

		// Verify the exact commit arrived at the selected remote.
		want, err := g.RevParse(ctx, "caic-0")
		if err != nil {
			t.Fatal(err)
		}
		got, err := (&Checkout{Root: bare, Logger: testLogger(t)}).RevParse(ctx, "refs/heads/caic-0")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("remote commit = %q, want %q", got, want)
		}
	})

	t.Run("SquashOnto", func(t *testing.T) {
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
	})

	t.Run("IsReachable", func(t *testing.T) {
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
	})

	t.Run("CreateBranch", func(t *testing.T) {
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
			if err := g.CreateBranch(ctx, "caic-1", "main", false); err != nil {
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

		t.Run("DoesNotTrackRemoteStartPoint", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bare := filepath.Join(t.TempDir(), "remote.git")

			run(t, "", "init", "--bare", "--initial-branch=main", bare)
			run(t, "", "clone", bare, dir)
			run(t, dir, "config", "user.name", "Test")
			run(t, dir, "config", "user.email", "test@test")
			run(t, dir, "config", "branch.autoSetupRebase", "always")
			run(t, dir, "commit", "--allow-empty", "-m", "init")
			run(t, dir, "push", "origin", "main")
			run(t, dir, "fetch", "origin")

			g := &Checkout{Root: dir, Logger: testLogger(t)}
			if err := g.CreateBranch(ctx, "caic-1", "origin/main", false); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, "git", "config", "--get-regexp", `^branch\.caic-1\.`)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("branch config = %q, want none", strings.TrimSpace(string(out)))
			}
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("git config --get-regexp: %v\n%s", err, out)
			}
		})

		t.Run("TracksRemoteStartPoint", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bare := filepath.Join(t.TempDir(), "remote.git")

			run(t, "", "init", "--bare", "--initial-branch=main", bare)
			run(t, "", "clone", bare, dir)
			run(t, dir, "config", "user.name", "Test")
			run(t, dir, "config", "user.email", "test@test")
			run(t, dir, "commit", "--allow-empty", "-m", "init")
			run(t, dir, "push", "origin", "main")
			run(t, dir, "fetch", "origin")

			g := &Checkout{Root: dir, Logger: testLogger(t)}
			if err := g.CreateBranch(ctx, "caic-1", "origin/main", true); err != nil {
				t.Fatal(err)
			}
			if got := run(t, dir, "config", "--get", "branch.caic-1.remote"); got != "origin" {
				t.Errorf("branch remote = %q, want origin", got)
			}
			if got := run(t, dir, "config", "--get", "branch.caic-1.merge"); got != "refs/heads/main" {
				t.Errorf("branch merge = %q, want refs/heads/main", got)
			}
		})

		t.Run("AlreadyExists", func(t *testing.T) { //nolint:paralleltest // subtests share git repo
			if err := g.CreateBranch(ctx, "caic-1", "main", false); err == nil {
				t.Error("expected error for duplicate branch")
			}
		})
	})

	t.Run("ObjectStats", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		run := func(args ...string) {
			cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // test-controlled args
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		run("init", "-q")
		run("config", "user.name", "Object Stats Test")
		run("config", "user.email", "object-stats@example.test")
		// Git truncates object sizes to KiB; use incompressible content so
		// the loose and packed sizes remain nonzero on every platform.
		content := make([]byte, 8192)
		if _, err := rand.Read(content); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", "file.txt")
		run("commit", "-qm", "first")
		g := &Checkout{Root: dir}
		stats, err := g.ObjectStats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.LooseCount < 3 || stats.LooseBytes <= 0 || stats.PackBytes != 0 {
			t.Fatalf("before repack: %+v", stats)
		}
		run("repack", "-ad")
		stats, err = g.ObjectStats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.LooseCount != 0 || stats.LooseBytes != 0 || stats.PackBytes <= 0 {
			t.Fatalf("after repack: %+v", stats)
		}
	})
}
