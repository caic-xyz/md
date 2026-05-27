// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Package gitutil provides git utility functions for repository introspection,
// branch management, and pushing.
package gitutil

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

// newGitCmd creates an exec.Cmd for git with LANG=C set so that output is
// always in English regardless of the system locale.
func newGitCmd(ctx context.Context, dir string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LANG=C")
	return cmd
}

// RunGit executes a git command in dir and returns captured stdout.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := newGitCmd(ctx, dir, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// RootDir returns the git repository root for the given working directory.
func RootDir(ctx context.Context, wd string) (string, error) {
	out, err := RunGit(ctx, wd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git checkout directory: %s: %w", wd, err)
	}
	return out, nil
}

// CurrentBranch returns the current branch name for the given working
// directory.
func CurrentBranch(ctx context.Context, wd string) (string, error) {
	out, err := RunGit(ctx, wd, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("check out a named branch: %w", err)
	}
	if out == "" {
		return "", errors.New("check out a named branch")
	}
	return out, nil
}

// DefaultRemote returns the default remote for the given working directory.
// If there is exactly one remote, it is returned. Otherwise "origin" is used.
func DefaultRemote(ctx context.Context, wd string) (string, error) {
	out, err := RunGit(ctx, wd, "remote")
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
// for the given remote in the given working directory.
func DefaultBranch(ctx context.Context, wd, remote string) (string, error) {
	prefix := "refs/remotes/" + remote + "/"
	// Try symbolic-ref first (works when <remote>/HEAD is set).
	if out, err := RunGit(ctx, wd, "symbolic-ref", prefix+"HEAD"); err == nil {
		if _, name, ok := strings.Cut(out, prefix); ok && name != "" {
			return name, nil
		}
	}
	// Fall back to checking common names.
	for _, name := range []string{"main", "master"} {
		if _, err := RunGit(ctx, wd, "rev-parse", "--verify", prefix+name); err == nil {
			return name, nil
		}
	}
	return "", errors.New("could not determine default branch")
}

// MergeBase returns the merge-base between HEAD and baseRef, falling back to
// baseRef itself if merge-base fails (e.g. unrelated histories).
func MergeBase(ctx context.Context, dir, baseRef string) string {
	if mb, err := RunGit(ctx, dir, "merge-base", "HEAD", baseRef); err == nil && mb != "" {
		return mb
	}
	return baseRef
}

// Fetch fetches the latest refs from origin.
func Fetch(ctx context.Context, dir string) error {
	slog.InfoContext(ctx, "git", "msg", "git fetch", "dir", dir)
	cmd := newGitCmd(ctx, dir, []string{"fetch", "origin"})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch origin: %w: %s", err, stderr.String())
	}
	return nil
}

// CreateBranch creates a new branch from startPoint without touching the
// working tree or index.
func CreateBranch(ctx context.Context, dir, name, startPoint string) error {
	slog.InfoContext(ctx, "git", "msg", "git create branch", "branch", name, "startPoint", startPoint)
	cmd := newGitCmd(ctx, dir, []string{"branch", name, startPoint})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git branch %s %s: %w: %s", name, startPoint, err, stderr.String())
	}
	return nil
}

// CheckoutBranch switches to an existing branch.
func CheckoutBranch(ctx context.Context, dir, name string) error {
	slog.InfoContext(ctx, "git", "msg", "git checkout", "branch", name)
	cmd := newGitCmd(ctx, dir, []string{"checkout", name})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", name, err, stderr.String())
	}
	return nil
}

// RemoteOriginURL returns the URL of the "origin" remote, or "" if
// unavailable.
func RemoteOriginURL(ctx context.Context, dir string) string {
	out, err := RunGit(ctx, dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return out
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

// PushRef pushes a local ref to the origin remote as the given branch.
// ref can be a remote-tracking ref (e.g. "container/branch"), a branch
// name, or any valid git ref. When force is true, --force is passed.
func PushRef(ctx context.Context, dir, ref, branch string, force bool) error {
	slog.InfoContext(ctx, "git", "msg", "git push", "ref", ref, "branch", branch, "force", force)
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "origin", ref+":refs/heads/"+branch)
	cmd := newGitCmd(ctx, dir, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push origin %s:%s: %w: %s", ref, branch, err, stderr.String())
	}
	return nil
}

// SquashOnto creates a single squash commit of sourceRef's tree on top of
// origin/<targetBranch> and pushes it. Uses plumbing commands only — no
// working-tree checkout needed. The push is non-force so it fails with a
// non-fast-forward error if origin/<targetBranch> has advanced since fetch.
func SquashOnto(ctx context.Context, dir, sourceRef, targetBranch, message string) error {
	slog.InfoContext(ctx, "git", "msg", "squash onto", "sourceRef", sourceRef, "targetBranch", targetBranch)

	// 1. Fetch so origin/<targetBranch> is up to date.
	if err := Fetch(ctx, dir); err != nil {
		return err
	}

	// 2. Create the squash commit: sourceRef's tree, parented on origin/<targetBranch>.
	target := "origin/" + targetBranch
	commitTreeArgs := []string{"commit-tree", "-p", target, "-m", message, sourceRef + "^{tree}"}
	cmd := newGitCmd(ctx, dir, commitTreeArgs)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git commit-tree: %w: %s", err, stderr.String())
	}
	newCommit := strings.TrimSpace(string(out))

	// 3. Push the new commit to origin/<targetBranch> (non-force).
	return PushRef(ctx, dir, newCommit, targetBranch, false)
}

// RevParse resolves a git ref to its full SHA-1 hash.
func RevParse(ctx context.Context, dir, ref string) (string, error) {
	return RunGit(ctx, dir, "rev-parse", "--verify", ref)
}

// IsReachable reports whether commit is an ancestor of (or equal to) any ref
// in refs/heads/ or refs/remotes/origin/. Container remote-tracking refs
// (refs/remotes/<container>/*) are excluded by construction.
func IsReachable(ctx context.Context, dir, commit string) (bool, error) {
	cmd := newGitCmd(ctx, dir, []string{
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
func ListBranches(ctx context.Context, dir, remote string) ([][2]string, error) {
	var refPath, prefix string
	if remote == "" {
		refPath = "refs/heads/"
	} else {
		refPath = "refs/remotes/" + remote + "/"
		prefix = remote + "/"
	}
	out, err := RunGit(ctx, dir, "for-each-ref", "--format=%(refname:short) %(objectname)", refPath)
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

// Submodule represents a git submodule declaration from .gitmodules.
type Submodule struct {
	Name string // submodule name (key in .gitmodules, usually equals Path)
	Path string // relative path in the worktree
}

// ListSubmodules returns the submodules declared in .gitmodules for the repo
// at dir. Returns nil if no .gitmodules file exists or it has no entries.
func ListSubmodules(ctx context.Context, dir string) ([]Submodule, error) {
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out, err := RunGit(ctx, dir, "config", "--file", ".gitmodules", "--list")
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

// FindModuleDirs returns relative paths (from <gitRoot>/.git/modules/) for
// every bare module repository found under that directory. Bare repos are
// detected by the presence of a HEAD file, an objects/ directory, and a
// refs/ directory. Nested module repos (submodules of submodules) stored
// under <module>/modules/ are included recursively.
// Returns nil if the modules directory does not exist.
func FindModuleDirs(gitRoot string) ([]string, error) {
	base := filepath.Join(gitRoot, ".git", "modules")
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil, nil
	}
	var paths []string
	if err := findModuleDirs(base, base, &paths); err != nil {
		return nil, err
	}
	slices.Sort(paths)
	return paths, nil
}

func findModuleDirs(base, dir string, paths *[]string) error {
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
			if err := findModuleDirs(base, filepath.Join(dir, "modules"), paths); err != nil {
				slog.Warn("git", "msg", "skipping nested modules", "dir", filepath.Join(dir, "modules"), "err", err)
			}
		}
		return nil
	}
	// Not a bare repo — recurse into all subdirectories.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := findModuleDirs(base, filepath.Join(dir, e.Name()), paths); err != nil {
			slog.Warn("git", "msg", "skipping subdirectory", "dir", filepath.Join(dir, e.Name()), "err", err)
		}
	}
	return nil
}

// DiscoverRepos recursively walks root up to maxDepth levels, returning
// absolute paths of git repositories. Both regular repos (containing a .git
// subdirectory) and bare repos (containing HEAD, objects/, and refs/ directly)
// are detected. Hidden directories (prefix ".") are skipped. Recursion stops
// once a repo is found.
func DiscoverRepos(root string, maxDepth int) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var repos []string
	err = discoverRepos(root, maxDepth, &repos)
	return repos, err
}

func discoverRepos(dir string, depth int, repos *[]string) error {
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
		if err := discoverRepos(filepath.Join(dir, e.Name()), depth-1, repos); err != nil {
			// Skip directories we can't read.
			continue
		}
	}
	return nil
}
