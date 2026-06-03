// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Package git provides git utility functions for repository introspection,
// branch management, and pushing.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// DiscoverCheckouts recursively walks root up to maxDepth levels, returning absolute paths of git
// repositories.
//
// Both regular repos (containing a .git subdirectory) and bare repos (containing HEAD, objects/, and refs/
// directly) are detected. Hidden directories (prefix ".") are skipped. Recursion stops once a repo is found,
// so submodules are not found.
func DiscoverCheckouts(root string, maxDepth int) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var repos []string
	err = discoverCheckouts(root, maxDepth, &repos)
	return repos, err
}

func discoverCheckouts(dir string, depth int, repos *[]string) error {
	if depth < 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	// Scan entries to detect regular (.git subdir) or bare repos (HEAD file +
	// objects/ + refs/ dirs).
	var hasGit, hasHEAD, hasObjects, hasRefs bool
	for _, e := range entries {
		switch e.Name() {
		case ".git":
			hasGit = true
		case "HEAD":
			hasHEAD = !e.IsDir()
		case "objects":
			hasObjects = e.IsDir()
		case "refs":
			hasRefs = e.IsDir()
		}
	}
	if hasGit || (hasHEAD && hasObjects && hasRefs) {
		*repos = append(*repos, dir)
		return nil // Don't recurse into repos.
	}
	// Recurse into subdirectories.
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := discoverCheckouts(filepath.Join(dir, e.Name()), depth-1, repos); err != nil {
			// Skip directories we can't read.
			continue
		}
	}
	return nil
}

// RemoteToHTTPS converts a git remote URL to an HTTPS browse URL.
// SSH (git@host:owner/repo.git), ssh:// and https:// with .git suffix are
// normalised. Unrecognised formats are returned as-is.
func RemoteToHTTPS(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// git@host:owner/repo.git → https://host/owner/repo
	if after, ok := strings.CutPrefix(raw, "git@"); ok {
		if i := strings.IndexByte(after, ':'); i > 0 {
			host := after[:i]
			path := strings.TrimSuffix(after[i+1:], ".git")
			return "https://" + host + "/" + path
		}
	}
	// ssh://git@host/owner/repo.git → https://host/owner/repo
	if after, ok := strings.CutPrefix(raw, "ssh://"); ok {
		// Strip user@ if present.
		if i := strings.IndexByte(after, '@'); i >= 0 {
			after = after[i+1:]
		}
		return "https://" + strings.TrimSuffix(after, ".git")
	}
	// https://host/owner/repo.git → strip .git
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		return strings.TrimSuffix(raw, ".git")
	}
	return raw
}

// Submodule represents a git submodule declaration from .gitmodules.
type Submodule struct {
	Name string // submodule name (key in .gitmodules, usually equals Path)
	Path string // relative path in the worktree
}

// Logger receives structured log records.
type Logger interface {
	Log(ctx context.Context, level slog.Level, msg string, args ...any)
}

// Checkout provides git operations scoped to the repository at Root, logging via
// Logger.
type Checkout struct {
	Root   string
	Logger Logger

	_ struct{}
}

// RootDir discovers the git repository root for the given working directory
// and returns a Git scoped to it.
func RootDir(ctx context.Context, wd string, logger Logger) (*Checkout, error) {
	g := &Checkout{Root: wd, Logger: logger}
	out, err := g.RunGit(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git checkout directory: %s: %w", wd, err)
	}
	g.Root = out
	return g, nil
}

// RunGit executes a git command in Root and returns captured stdout.
func (c *Checkout) RunGit(ctx context.Context, args ...string) (string, error) {
	cmd := c.cmd(ctx, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentBranch returns the current branch name for Root.
func (c *Checkout) CurrentBranch(ctx context.Context) (string, error) {
	out, err := c.RunGit(ctx, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("check out a named branch: %w", err)
	}
	if out == "" {
		return "", errors.New("check out a named branch")
	}
	return out, nil
}

// DefaultRemote returns the default remote for Root.
// If there is exactly one remote, it is returned. Otherwise "origin" is used.
func (c *Checkout) DefaultRemote(ctx context.Context) (string, error) {
	out, err := c.RunGit(ctx, "remote")
	if err != nil || out == "" {
		return "", errors.New("no git remotes configured")
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 1 {
		return lines[0], nil
	}
	if slices.Contains(lines, "origin") {
		return "origin", nil
	}
	return "", fmt.Errorf("multiple remotes and no %q", "origin")
}

// DefaultBranch returns the default branch name (e.g. "main" or "master")
// for the given remote in Root.
func (c *Checkout) DefaultBranch(ctx context.Context, remote string) (string, error) {
	prefix := "refs/remotes/" + remote + "/"
	// Try symbolic-ref first (works when <remote>/HEAD is set).
	if out, err := c.RunGit(ctx, "symbolic-ref", prefix+"HEAD"); err == nil {
		if _, name, ok := strings.Cut(out, prefix); ok && name != "" {
			return name, nil
		}
	}
	// Fall back to checking common names.
	for _, name := range []string{"main", "master"} {
		if _, err := c.RunGit(ctx, "rev-parse", "--verify", prefix+name); err == nil {
			return name, nil
		}
	}
	return "", errors.New("could not determine default branch")
}

// MergeBase returns the merge-base between HEAD and baseRef, falling back to
// baseRef itself if merge-base fails (e.g. unrelated histories).
func (c *Checkout) MergeBase(ctx context.Context, baseRef string) string {
	if mb, err := c.RunGit(ctx, "merge-base", "HEAD", baseRef); err == nil && mb != "" {
		return mb
	}
	return baseRef
}

// Fetch fetches the latest refs from origin.
func (c *Checkout) Fetch(ctx context.Context) error {
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "git fetch", "dir", c.Root)
	cmd := c.cmd(ctx, []string{"fetch", "origin"})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch origin: %w: %s", err, stderr.String())
	}
	return nil
}

// CreateBranch creates a new branch from startPoint without touching the
// working tree or index.
func (c *Checkout) CreateBranch(ctx context.Context, name, startPoint string, track bool) error {
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "git create branch", "branch", name, "startPoint", startPoint, "track", track)
	trackArg := "--no-track"
	if track {
		trackArg = "--track"
	}
	cmd := c.cmd(ctx, []string{"branch", trackArg, name, startPoint})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git branch %s %s %s: %w: %s", trackArg, name, startPoint, err, stderr.String())
	}
	return nil
}

// CheckoutBranch switches to an existing branch.
func (c *Checkout) CheckoutBranch(ctx context.Context, name string) error {
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "git checkout", "branch", name)
	cmd := c.cmd(ctx, []string{"checkout", name})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", name, err, stderr.String())
	}
	return nil
}

// RemoteOriginURL returns the URL of the "origin" remote, or "" if
// unavailable.
func (c *Checkout) RemoteOriginURL(ctx context.Context) string {
	out, err := c.RunGit(ctx, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return out
}

// PushRef pushes a local ref to the origin remote as the given branch.
// ref can be a remote-tracking ref (e.g. "container/branch"), a branch
// name, or any valid git ref. When force is true, --force is passed.
func (c *Checkout) PushRef(ctx context.Context, ref, branch string, force bool) error {
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "git push", "ref", ref, "branch", branch, "force", force)
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "origin", ref+":refs/heads/"+branch)
	cmd := c.cmd(ctx, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push origin %s:%s: %w: %s", ref, branch, err, stderr.String())
	}
	return nil
}

// SquashOnto creates a single squash commit of sourceRef's tree on top of
// origin/<targetBranch> and pushes it.
//
// Uses plumbing commands only, no active branch checkout needed. The push is non-force so it fails with a
// non-fast-forward error if origin/<targetBranch> has advanced since fetch.
func (c *Checkout) SquashOnto(ctx context.Context, sourceRef, targetBranch, message string) error {
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "squash onto", "sourceRef", sourceRef, "targetBranch", targetBranch)

	// 1. Fetch so origin/<targetBranch> is up to date.
	if err := c.Fetch(ctx); err != nil {
		return err
	}

	// 2. Create the squash commit: sourceRef's tree, parented on origin/<targetBranch>.
	target := "origin/" + targetBranch
	commitTreeArgs := []string{"commit-tree", "-p", target, "-m", message, sourceRef + "^{tree}"}
	cmd := c.cmd(ctx, commitTreeArgs)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git commit-tree: %w: %s", err, stderr.String())
	}
	newCommit := strings.TrimSpace(string(out))

	// 3. Push the new commit to origin/<targetBranch> (non-force).
	return c.PushRef(ctx, newCommit, targetBranch, false)
}

// RevParse resolves a git ref to its full SHA-1 hash.
func (c *Checkout) RevParse(ctx context.Context, ref string) (string, error) {
	return c.RunGit(ctx, "rev-parse", "--verify", ref)
}

// IsReachable reports whether commit is an ancestor of (or equal to) any ref
// in refs/heads/ or refs/remotes/origin/. Container remote-tracking refs
// (refs/remotes/<container>/*) are excluded by construction.
func (c *Checkout) IsReachable(ctx context.Context, commit string) (bool, error) {
	cmd := c.cmd(ctx, []string{
		"for-each-ref",
		"--contains", commit,
		"--format=%(refname)",
		"refs/heads/",
		"refs/remotes/origin/",
	})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git for-each-ref --contains %s: %w: %s", commit, err, stderr.String())
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// ListBranches returns branches sorted alphabetically. It always runs git
// directly with no caching so the result is always fresh even when branches
// are created or deleted frequently.
//
// If remote is empty, local branches (refs/heads/) are listed and each entry's
// first element is the short branch name. If remote is non-empty, remote
// tracking branches for that remote (refs/remotes/<remote>/) are listed and
// HEAD is excluded.
func (c *Checkout) ListBranches(ctx context.Context, remote string) ([][2]string, error) {
	var refPath, prefix string
	if remote == "" {
		refPath = "refs/heads/"
	} else {
		refPath = "refs/remotes/" + remote + "/"
		prefix = remote + "/"
	}
	out, err := c.RunGit(ctx, "for-each-ref", "--format=%(refname:short) %(objectname)", refPath)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	branches := make([][2]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		ref, hash, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if prefix != "" {
			name, cut := strings.CutPrefix(ref, prefix)
			if !cut || name == "HEAD" {
				continue
			}
			ref = name
		}
		branches = append(branches, [2]string{ref, hash})
	}
	slices.SortFunc(branches, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return branches, nil
}

// ListSubmodules returns the submodules declared in .gitmodules for Root.
// Returns nil if no .gitmodules file exists or it has no entries.
func (c *Checkout) ListSubmodules(ctx context.Context) ([]Submodule, error) {
	if _, err := os.Stat(filepath.Join(c.Root, ".gitmodules")); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out, err := c.RunGit(ctx, "config", "--file", ".gitmodules", "--list")
	if err != nil {
		return nil, err
	}
	byName := map[string]*Submodule{}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// key is "submodule.<name>.<field>"
		after, fieldOK := strings.CutPrefix(key, "submodule.")
		if !fieldOK {
			continue
		}
		dot := strings.LastIndex(after, ".")
		if dot < 0 {
			continue
		}
		name, field := after[:dot], after[dot+1:]
		if field != "path" {
			continue
		}
		if _, exists := byName[name]; !exists {
			byName[name] = &Submodule{Name: name}
		}
		byName[name].Path = val
	}
	subs := make([]Submodule, 0, len(byName))
	for _, s := range byName {
		if s.Path != "" {
			subs = append(subs, *s)
		}
	}
	slices.SortFunc(subs, func(a, b Submodule) int { return strings.Compare(a.Name, b.Name) })
	return subs, nil
}

// FindModuleDirs returns relative paths (from <Root>/.git/modules/) for
// every bare module repository found under that directory.
//
// Bare repos are detected by the presence of a HEAD file, an objects/ directory, and a refs/ directory.
// Nested module repos (submodules of submodules) stored under <module>/modules/ are included recursively.
// Returns nil if the modules directory does not exist.
func (c *Checkout) FindModuleDirs(ctx context.Context) ([]string, error) {
	base := filepath.Join(c.Root, ".git", "modules")
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil, nil
	}
	var paths []string
	if err := c.findModuleDirs(ctx, base, base, &paths); err != nil {
		return nil, err
	}
	slices.Sort(paths)
	return paths, nil
}

// cmd creates an exec.Cmd for git with LANG=C set so that output is always in
// English regardless of the system locale.
func (c *Checkout) cmd(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from trusted callers
	cmd.Dir = c.Root
	cmd.Env = append(os.Environ(), "LANG=C")
	return cmd
}

func (c *Checkout) findModuleDirs(ctx context.Context, base, dir string, paths *[]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	var hasHEAD, hasObjects, hasRefs, hasModules bool
	for _, e := range entries {
		switch e.Name() {
		case "HEAD":
			hasHEAD = !e.IsDir()
		case "objects":
			hasObjects = e.IsDir()
		case "refs":
			hasRefs = e.IsDir()
		case "modules":
			hasModules = e.IsDir()
		}
	}
	if hasHEAD && hasObjects && hasRefs {
		rel, err := filepath.Rel(base, dir)
		if err != nil {
			return err
		}
		*paths = append(*paths, filepath.ToSlash(rel))
		// Recurse into <module>/modules/ to find nested submodule repos.
		if hasModules {
			if err := c.findModuleDirs(ctx, base, filepath.Join(dir, "modules"), paths); err != nil {
				c.Logger.Log(ctx, slog.LevelWarn, "git", "msg", "skipping nested modules", "dir", filepath.Join(dir, "modules"), "err", err)
			}
		}
		return nil
	}
	// Not a bare repo — recurse into all subdirectories.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := c.findModuleDirs(ctx, base, filepath.Join(dir, e.Name()), paths); err != nil {
			c.Logger.Log(ctx, slog.LevelWarn, "git", "msg", "skipping subdirectory", "dir", filepath.Join(dir, e.Name()), "err", err)
		}
	}
	return nil
}
