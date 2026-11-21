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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

// ObjectStats describes the loose and packed objects in a Git checkout.
type ObjectStats struct {
	LooseCount int64
	LooseBytes int64
	PackBytes  int64
}

// Checkout scopes Git operations to a worktree or explicit repository metadata,
// logging via Logger.
type Checkout struct {
	// Root scopes worktree files. With GitDir set it may name a removed
	// worktree. Use [ObjectStore] when working with repository objects alone.
	Root string
	// GitDir explicitly selects an absolute repository metadata directory.
	// It overrides core.worktree without changing the repository's config.
	// Filesystem-based checkout operations still require a usable Root.
	//
	// Deprecated: use [ObjectStore].
	GitDir string
	Logger Logger

	_ struct{}
}

// RunGit executes a scoped Git command and returns captured stdout.
func (c *Checkout) RunGit(ctx context.Context, args ...string) (string, error) {
	return runGit(c.Command(ctx, args...), args)
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

// RefExists reports whether the fully-qualified ref (e.g. "refs/heads/foo")
// exists in Root.
func (c *Checkout) RefExists(ctx context.Context, ref string) (bool, error) {
	cmd := c.Command(ctx, "show-ref", "--verify", "--quiet", ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git show-ref --verify --quiet %s: %w: %s", ref, err, stderr.String())
	}
	return true, nil
}

// Remotes returns the configured remote names sorted alphabetically.
func (c *Checkout) Remotes(ctx context.Context) ([]string, error) {
	return remotes(ctx, c.RunGit)
}

// DefaultRemote returns the default remote for Root.
// If there is exactly one remote, it is returned. Otherwise "origin" is used.
func (c *Checkout) DefaultRemote(ctx context.Context) (string, error) {
	return defaultRemote(ctx, c.RunGit)
}

// DefaultBranch returns the default branch name (e.g. "main" or "master")
// for the given remote in Root.
func (c *Checkout) DefaultBranch(ctx context.Context, remote string) (string, error) {
	return defaultBranch(ctx, c.RunGit, remote)
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
	cmd := c.Command(ctx, "fetch", "origin")
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
	cmd := c.Command(ctx, "branch", trackArg, name, startPoint)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git branch %s %s %s: %w: %s", trackArg, name, startPoint, err, stderr.String())
	}
	return nil
}

// CheckoutBranch switches to an existing branch in an existing worktree.
func (c *Checkout) CheckoutBranch(ctx context.Context, name string) error {
	if c.GitDir != "" {
		if err := c.validateWorktreeRoot(); err != nil {
			return err
		}
	}
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "git checkout", "branch", name)
	cmd := c.Command(ctx, "checkout", name)
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

// PushRef pushes sourceRef to destinationBranch on the named remote.
//
// sourceRef can be HEAD, a commit ID, or a local or remote-tracking ref.
// destinationBranch is a branch name, without the refs/heads/ prefix.
// When force is true, --force is passed.
func (c *Checkout) PushRef(ctx context.Context, remote, sourceRef, destinationBranch string, force bool) error {
	c.Logger.Log(ctx, slog.LevelInfo, "git", "msg", "git push", "remote", remote, "sourceRef", sourceRef, "destinationBranch", destinationBranch, "force", force)
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", remote, sourceRef+":refs/heads/"+destinationBranch)
	cmd := c.Command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push %s %s:%s: %w: %s", remote, sourceRef, destinationBranch, err, stderr.String())
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
	cmd := c.Command(ctx, commitTreeArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git commit-tree: %w: %s", err, stderr.String())
	}
	newCommit := strings.TrimSpace(string(out))

	// 3. Push the new commit to origin/<targetBranch> (non-force).
	return c.PushRef(ctx, "origin", newCommit, targetBranch, false)
}

// RevParse resolves a git ref to its full SHA-1 hash.
func (c *Checkout) RevParse(ctx context.Context, ref string) (string, error) {
	return revParse(ctx, c.RunGit, ref)
}

// IsReachable reports whether commit is an ancestor of (or equal to) any ref
// in refs/heads/ or refs/remotes/origin/. Container remote-tracking refs
// (refs/remotes/<container>/*) are excluded by construction.
func (c *Checkout) IsReachable(ctx context.Context, commit string) (bool, error) {
	cmd := c.Command(ctx, "for-each-ref", "--contains", commit,
		"--format=%(refname)", "refs/heads/", "refs/remotes/origin/")
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
// Returns nil if no .gitmodules file exists or it has no entries. An explicit
// GitDir scope requires an existing Root to inspect checkout files.
func (c *Checkout) ListSubmodules(ctx context.Context) ([]Submodule, error) {
	if c.GitDir != "" {
		if err := c.validateWorktreeRoot(); err != nil {
			return nil, err
		}
	}
	file, err := filepath.Abs(filepath.Join(c.Root, ".gitmodules"))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(file); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out, err := c.RunGit(ctx, "config", "--file", filepath.ToSlash(file), "--list")
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
		name, field, ok := strings.CutLast(after, ".")
		if !ok {
			continue
		}
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

// ObjectStats reads loose-object count and object-store sizes from Git.
// Git reports size and size-pack in KiB; the returned sizes are bytes.
func (c *Checkout) ObjectStats(ctx context.Context) (ObjectStats, error) {
	out, err := c.RunGit(ctx, "count-objects", "-v")
	if err != nil {
		return ObjectStats{}, err
	}
	var stats ObjectStats
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch key {
		case "count":
			stats.LooseCount = n
		case "size":
			if n <= math.MaxInt64/1024 {
				stats.LooseBytes = n * 1024
			}
		case "size-pack":
			if n <= math.MaxInt64/1024 {
				stats.PackBytes = n * 1024
			}
		}
	}
	return stats, nil
}

// Command creates a scoped Git command with English output. Callers may
// configure its I/O before running it, including streaming large output.
//
// With GitDir set, the command starts in that directory and explicitly selects
// its repository metadata and Root worktree, bypassing stale core.worktree
// configuration. An empty Root uses GitDir as the command's worktree scope for
// object-only operations during migration to [ObjectStore]. Root and GitDir
// must be absolute in this mode.
func (c *Checkout) Command(ctx context.Context, args ...string) *exec.Cmd {
	dir := c.Root
	if c.GitDir != "" {
		dir = c.GitDir
		worktree := c.Root
		if worktree == "" {
			worktree = c.GitDir
		}
		args = append([]string{"--git-dir=" + filepath.ToSlash(c.GitDir), "--work-tree=" + filepath.ToSlash(worktree)}, args...)
	}
	return gitCommand(ctx, dir, args...)
}

// validateWorktreeRoot validates only operations that need checkout files. Object and
// ref commands can also use [ObjectStore].
func (c *Checkout) validateWorktreeRoot() error {
	root := c.Root
	if root == "" {
		if c.GitDir != "" {
			return errors.New("a worktree Root is required for this operation")
		}
		root = "."
	}
	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("worktree Root %q: %w", root, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("worktree Root %q is not a directory", root)
	}
	return nil
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

// ObjectStore scopes Git commands to an existing repository's metadata, refs,
// and objects. It works with bare repositories and retained Git directories
// whose checkout is missing or has moved.
type ObjectStore struct {
	// GitDir is the absolute path to the repository's Git directory.
	GitDir string
	// Env supplies the process environment. If nil, commands inherit it.
	Env []string
}

// Command creates a Git command with English output that starts in GitDir.
// It runs Git in bare mode without changing the repository configuration.
// Commands such as status and checkout require a worktree and fail in this
// mode. Commands can still act on an explicit destination, for example with
// worktree add. Callers may configure the command's I/O before running it.
func (s *ObjectStore) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := gitCommand(ctx, s.GitDir, append([]string{"--git-dir=" + filepath.ToSlash(s.GitDir), "-c", "core.bare=true"}, args...)...)
	if !filepath.IsAbs(s.GitDir) {
		cmd.Err = fmt.Errorf("GitDir %q must be absolute", s.GitDir)
		return cmd
	}
	// An explicit GIT_COMMON_DIR makes Git ignore core.worktree, even
	// when the configured path is relative and its directory is gone.
	commonDir := s.GitDir
	commonFile := filepath.Join(s.GitDir, "commondir")
	if data, err := os.ReadFile(commonFile); err == nil { //nolint:gosec // GitDir selects the caller's repository.
		commonDir = strings.TrimRight(string(data), "\r\n")
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(s.GitDir, commonDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cmd.Err = fmt.Errorf("read Git common directory: %w", err)
		return cmd
	}
	if s.Env != nil {
		cmd.Env = slices.Clone(s.Env)
	}
	cmd.Env = slices.DeleteFunc(cmd.Env, func(env string) bool {
		key, _, _ := strings.Cut(env, "=")
		return strings.EqualFold(key, "GIT_WORK_TREE")
	})
	cmd.Env = append(cmd.Env, "LANG=C", "GIT_COMMON_DIR="+filepath.ToSlash(commonDir), "GIT_IMPLICIT_WORK_TREE=0")
	return cmd
}

// RunGit executes a Git command in the object store and returns captured stdout.
func (s *ObjectStore) RunGit(ctx context.Context, args ...string) (string, error) {
	return runGit(s.Command(ctx, args...), args)
}

// RevParse resolves a Git revision to its full object ID.
func (s *ObjectStore) RevParse(ctx context.Context, ref string) (string, error) {
	return revParse(ctx, s.RunGit, ref)
}

// DefaultRemote returns the only configured remote or selects origin if several
// remotes are configured. It returns an error if neither rule applies.
func (s *ObjectStore) DefaultRemote(ctx context.Context) (string, error) {
	return defaultRemote(ctx, s.RunGit)
}

// DefaultBranch resolves the default branch from the remote-tracking HEAD, or
// falls back to the first existing remote-tracking branch among main and master.
func (s *ObjectStore) DefaultBranch(ctx context.Context, remote string) (string, error) {
	return defaultBranch(ctx, s.RunGit, remote)
}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LANG=C")
	return cmd
}

func runGit(cmd *exec.Cmd, args []string) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

type gitRunner func(context.Context, ...string) (string, error)

func remotes(ctx context.Context, run gitRunner) ([]string, error) {
	out, err := run(ctx, "remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, errors.New("no git remotes configured")
	}
	return strings.Split(out, "\n"), nil
}

func defaultRemote(ctx context.Context, run gitRunner) (string, error) {
	names, err := remotes(ctx, run)
	if err != nil {
		return "", err
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if slices.Contains(names, "origin") {
		return "origin", nil
	}
	return "", fmt.Errorf("multiple remotes and no %q", "origin")
}

func defaultBranch(ctx context.Context, run gitRunner, remote string) (string, error) {
	prefix := "refs/remotes/" + remote + "/"
	if out, err := run(ctx, "symbolic-ref", prefix+"HEAD"); err == nil {
		if _, name, ok := strings.Cut(out, prefix); ok && name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if _, err := revParse(ctx, run, prefix+name); err == nil {
			return name, nil
		}
	}
	return "", errors.New("could not determine default branch")
}

func revParse(ctx context.Context, run gitRunner, ref string) (string, error) {
	return run(ctx, "rev-parse", "--verify", ref)
}
