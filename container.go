// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Container lifecycle and configuration types.

package md

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/maruel/genai"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"github.com/caic-xyz/md/containers"
	"github.com/caic-xyz/md/git"
)

// DefaultBaseImage is the base image used when none is specified.
const DefaultBaseImage = "ghcr.io/caic-xyz/md-user"

// ErrDiffFound is returned by Container.Diff when its Git arguments request an
// exit status for differences and at least one difference is found.
var ErrDiffFound = errors.New("differences found")

const (
	// gitDiffCommand reserves this remote exit status so Container.Diff can
	// distinguish differences from command failures, including failures that
	// happen to exit with Git's ordinary difference status of 1.
	diffFoundSSHExitCode = 79
	maxPushRefspecBytes  = 16 * 1024
)

// Values for the "md.image_type" label, which tags md-built images with their
// role so they can be found for pruning, including after they are untagged.
const (
	// imageTypeSpecialized marks a per-user specialized image built on top of a
	// base image with SSH keys and caches baked in (md-specialized-<hash>).
	imageTypeSpecialized = "specialized"
	// imageTypeForkSnapshot marks a transient snapshot committed from a running
	// container to launch a fork. It is untagged once the fork starts, leaving a
	// dangling image discoverable only by this label.
	imageTypeForkSnapshot = "fork-snapshot"
)

const (
	tailscaleDeviceIDPath = "/var/lib/md/tailscale_device_id"

	// containerHomeDir is the container user's home directory.
	containerHomeDir = "/home/user"
	// The image builds user as the conventional first regular Linux account at
	// 1000:1000. Rootless Podman maps the host user to this container identity;
	// these values are independent of the host user's UID and GID.
	containerUserUID = 1000
	containerUserGID = 1000

	// Runtime images can contain large repos/caches under /home/user/src;
	// start.sh may need to repair ownership before SSH is ready.
	containerSSHReadyTimeout        = 2 * time.Minute
	containerSSHCommandRetryTimeout = 30 * time.Second

	revokeSudoCommand = `if id -nG user | tr ' ' '\n' | grep -qx sudo; then
	deluser user sudo >/dev/null 2>&1 || true
fi
if id -nG user | tr ' ' '\n' | grep -qx sudo; then
	echo "user remains in sudo group"
	exit 1
fi
passwd -l user >/dev/null`

	// gitBaseRefCommand resolves base_ref and diff_base_ref for container diffs.
	// It resolves the current upstream, falls back to the upstream of the branch
	// recorded by an in-progress rebase, supports the legacy base branch, then
	// uses the merge-base with HEAD for diff_base_ref when possible.
	gitBaseRefCommandPrefix = `if ! base_ref=$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null); then
	base_ref=
fi
if [ -z "$base_ref" ]; then
	for rebase_head in "$(git rev-parse --git-path rebase-merge/head-name)" "$(git rev-parse --git-path rebase-apply/head-name)"; do
		if [ -s "$rebase_head" ]; then
			head_ref=$(cat "$rebase_head")
			base_ref=$(git for-each-ref --format='%(upstream:short)' "$head_ref")
			break
		fi
	done
fi
if [ -z "$base_ref" ] && git rev-parse --verify --quiet base >/dev/null; then
	base_ref=base
fi
if [ -z "$base_ref" ]; then
`
	gitBaseRefCommandSuffix = `fi
diff_base_ref=$base_ref
if merge_base=$(git merge-base HEAD "$base_ref" 2>/dev/null) && [ -n "$merge_base" ]; then
	diff_base_ref=$merge_base
fi`

	// containerSyncRefPrefix namespaces the container-side ref recording the
	// container commit most recently integrated into or established as the
	// mapped host branch. This is the base "md diff" reports from by default.
	//
	// Start and Push write these from host branch refs; Pull writes the exact
	// container commits it integrated. Fetch deliberately leaves them unchanged.
	// Git keeps the commits reachable, so the base survives the agent amending,
	// resetting or rebasing the branch afterwards. The refs live outside
	// refs/heads/, so they are neither branches nor matched by mapped branch
	// refspecs. They are separate from refs/md/incoming, which temporarily names
	// host branch tips used to seed container branches.
	containerSyncRefPrefix     = "refs/md/sync/"
	containerIncomingRefPrefix = "refs/md/incoming/"
)

func gitBaseRefCommand() string {
	return gitBaseRefCommandPrefix + "\techo 'no upstream branch configured' >&2\n\texit 128\n" + gitBaseRefCommandSuffix
}

// gitIntegrationBaseRefCommand resolves diff_base_ref to the container commit
// most recently integrated into or established as the mapped host branch,
// which is what "md diff" reports by default.
//
// It falls back to gitDiffBaseRefCommand when that branch has no recorded sync
// point, as happens on a branch the agent created itself.
//
// Both cases report the base they chose on stderr. quiet drops those notes, as
// "git diff --quiet" asks for no output at all.
func gitIntegrationBaseRefCommand(primaryBranch, defaultRemote, defaultBranch string, quiet bool) string {
	// The upstream tip being reachable from HEAD but not from the integration
	// point means the branch moved onto upstream commits the point predates, so
	// the diff spans them. Replayed copies of those commits keep distinct
	// object IDs and stay undetected, hence "may".
	rebasedNote := `	upstream_ref=$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null) || upstream_ref=
	if [ -n "$upstream_ref" ] && git merge-base --is-ancestor "$upstream_ref" HEAD 2>/dev/null && ! git merge-base --is-ancestor "$upstream_ref" "$integration_base" 2>/dev/null; then
		echo 'note: the branch moved onto a newer upstream since its last host integration, so this diff may also show upstream commits; use md diff -full for the whole branch.' >&2
	fi
`
	missingNote := "\t" + `if [ -n "$current_branch" ]; then echo "note: branch \"$current_branch\" has no recorded host integration point, so this diff shows the whole branch." >&2; fi` + "\n"
	if quiet {
		rebasedNote = ""
		missingNote = ""
	}
	// An interrupted rebase leaves HEAD detached, so recover the branch being
	// rebased the way gitBaseRefCommandPrefix recovers its upstream. Without
	// this the integration point is missed for exactly the branch the agent is
	// rewriting.
	return `current_branch=$(git branch --show-current)
if [ -z "$current_branch" ]; then
	for rebase_head in "$(git rev-parse --git-path rebase-merge/head-name)" "$(git rev-parse --git-path rebase-apply/head-name)"; do
		if [ -s "$rebase_head" ]; then
			rebase_branch=$(cat "$rebase_head")
			current_branch=${rebase_branch#refs/heads/}
			break
		fi
	done
fi
integration_base=
if [ -n "$current_branch" ]; then
	integration_base=$(git rev-parse --verify -q "` + containerSyncRefPrefix + `$current_branch") || integration_base=
fi
if [ -n "$integration_base" ]; then
	diff_base_ref=$integration_base
` + rebasedNote + `else
` + gitDiffBaseRefCommand(primaryBranch, defaultRemote, defaultBranch) + `
` + missingNote + `fi`
}

func gitDiffBaseRefCommand(primaryBranch, defaultRemote, defaultBranch string) string {
	primaryBranchRef := shellQuote(primaryBranch)
	primaryBranchCommand := shellQuote(primaryBranch)
	defaultBranchCommand := shellQuote(defaultRemote + "/" + defaultBranch)
	return gitBaseRefCommandPrefix + `	current_branch=$(git branch --show-current)
	if [ -z "$current_branch" ]; then
		echo 'cannot run md diff: HEAD is detached and has no upstream branch configured' >&2
		echo ` + shellQuote("Switch to the container's primary branch: git switch "+primaryBranchCommand) + ` >&2
	elif [ "$current_branch" = ` + primaryBranchRef + ` ]; then
		echo ` + shellQuote(`primary branch "`+primaryBranch+`" has no upstream branch configured`) + ` >&2
		echo ` + shellQuote("Configure it: git branch --set-upstream-to="+defaultBranchCommand+" "+primaryBranchCommand) + ` >&2
	else
		echo "current branch \"$current_branch\" has no upstream branch configured" >&2
		echo 'md diff compares the checked-out branch with its upstream.' >&2
		echo ` + shellQuote(`The container's primary branch is "`+primaryBranch+`".`) + ` >&2
		echo ` + shellQuote("Either switch back: git switch "+primaryBranchCommand) + ` >&2
		echo ` + shellQuote("Or configure this branch: git branch --set-upstream-to="+primaryBranchCommand) + ` >&2
	fi
	exit 128
` + gitBaseRefCommandSuffix
}

var forkSnapshotEnvKeys = [...]string{
	"MD_DISPLAY",
	"MD_HOST_GID",
	"MD_HOST_UID",
	"MD_SUDO_PASSWORD",
	"MD_TAILSCALE",
	"MD_TAILSCALE_EPHEMERAL",
	"MD_TAILSCALE_RESET",
	// TODO: Rename TAILSCALE_AUTHKEY with an MD_ prefix so fork snapshots
	// can clear all MD_* runtime control env values generically.
	"TAILSCALE_AUTHKEY",
}

// Repo describes a git repository to push into a container.
type Repo struct {
	// GitRoot is the absolute path to the git repository root on the host.
	GitRoot string `json:"git_root"`
	// Branches lists the git branches to push into the container.
	// Branches[0] is the primary branch (checked out in the working tree,
	// used for container naming). Branches[1:] are also available as local
	// branches. Each branch tracks the same upstream as its host branch.
	Branches []string `json:"branches"`
	// ContainerPath is the absolute destination path inside the
	// container, e.g. "/home/user/src/github/caic". When empty,
	// populateContainerPath fills it from filepath.Base(GitRoot).
	// When two repos share the same basename, resolveContainerPaths
	// disambiguates using relative paths from /home/user/src.
	// Callers may set it explicitly to override.
	ContainerPath string `json:"container_path,omitempty"`
	// Remotes lists the host remotes whose cached branches are mirrored into the container.
	Remotes []string `json:"remotes,omitempty"`
	// TagRegexp selects host tags to mirror into the container. Empty selects no tags.
	TagRegexp string `json:"tag_regexp,omitempty"`
	// DefaultRemote is the host's default git remote. When empty, the upstream
	// remote tracked by local main or master takes precedence over origin.
	DefaultRemote string `json:"default_remote,omitempty"`
	// DefaultBranch is the default branch for DefaultRemote.
	DefaultBranch string `json:"default_branch,omitempty"`
}

// UnmarshalJSON decodes Repo, including legacy persisted labels.
func (r *Repo) UnmarshalJSON(data []byte) error {
	type repoJSON Repo
	var raw repoJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = Repo(raw)
	if r.ContainerPath == "" {
		var legacy struct {
			MountedPath string `json:"mounted_path"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		r.ContainerPath = legacy.MountedPath
	}
	if len(r.Branches) == 0 {
		// TODO: Remove legacy branch label support by 2026-09-01.
		var legacy struct {
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		if legacy.Branch != "" {
			r.Branches = []string{legacy.Branch}
		}
	}
	return nil
}

// Validate returns an error for invalid repo fields.
func (r *Repo) Validate() error {
	r.populateContainerPath()
	r.ContainerPath = ResolveContainerPath(r.ContainerPath)
	if r.GitRoot == "" {
		return errors.New("Repo.GitRoot is empty")
	}
	if len(r.Branches) == 0 {
		return errors.New("Repo.Branches is empty")
	}
	for i, remote := range r.Remotes {
		if remote == "" {
			return errors.New("Repo.Remotes contains an empty remote")
		}
		if strings.ContainsFunc(remote, unicode.IsSpace) {
			return fmt.Errorf("Repo.Remotes contains remote %q with whitespace", remote)
		}
		if slices.Contains(r.Remotes[i+1:], remote) {
			return fmt.Errorf("Repo.Remotes contains duplicate remote %q", remote)
		}
	}
	if len(r.Remotes) != 0 && r.DefaultRemote != "" && !slices.Contains(r.Remotes, r.DefaultRemote) {
		return fmt.Errorf("Repo.DefaultRemote %q is not in Repo.Remotes", r.DefaultRemote)
	}
	if r.TagRegexp != "" {
		if _, err := regexp.Compile(r.TagRegexp); err != nil {
			return fmt.Errorf("Repo.TagRegexp contains invalid tag regexp %q: %w", r.TagRegexp, err)
		}
	}
	for i, branch := range r.Branches {
		if branch == "" {
			return errors.New("Repo.Branches contains an empty branch")
		}
		if strings.ContainsFunc(branch, unicode.IsSpace) {
			return fmt.Errorf("Repo.Branches contains branch %q with whitespace", branch)
		}
		if slices.Contains(r.Branches[i+1:], branch) {
			return fmt.Errorf("Repo.Branches contains duplicate branch %q", branch)
		}
	}
	if r.ContainerPath == "" {
		return errors.New("Repo.ContainerPath could not be determined from GitRoot")
	}
	if !path.IsAbs(r.ContainerPath) {
		return fmt.Errorf("Repo.ContainerPath must be an absolute POSIX path, got %q", r.ContainerPath)
	}
	return nil
}

// resolveRemotes records external remotes, excluding the named remote and any
// md container transport remote that targets this repository's mounted path.
func (r *Repo) resolveRemotes(ctx context.Context, logger *slog.Logger, excludedRemote string) error {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	remotes, err := g.Remotes(ctx)
	if err != nil {
		return err
	}
	r.Remotes = r.Remotes[:0]
	for _, remote := range remotes {
		if remote == excludedRemote {
			continue
		}
		if r.isMDTransportRemote(ctx, g, remote) {
			continue
		}
		r.Remotes = append(r.Remotes, remote)
	}
	return nil
}

// isMDTransportRemote reports whether remote is an md SSH transport targeting
// this repository. URL lookup errors return false because retaining an unknown
// remote is safer than discarding potentially useful cached refs or legacy
// configuration.
func (r *Repo) isMDTransportRemote(ctx context.Context, g *git.Checkout, remote string) bool {
	if remote == "" || remote == "." {
		return false
	}
	remoteURL, err := g.RunGit(ctx, "remote", "get-url", remote)
	return err == nil && remoteURL == "user@"+remote+":"+r.ContainerPath
}

// resolveDefaults populates Remotes, DefaultRemote, and DefaultBranch if not already set.
func (r *Repo) resolveDefaults(ctx context.Context, logger *slog.Logger) error {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	if len(r.Remotes) == 0 && r.DefaultRemote == "" {
		if err := r.resolveRemotes(ctx, logger, ""); err != nil {
			return err
		}
	}
	var upstreamEvidenceErr error
	if r.DefaultRemote == "" {
		// A local default branch's upstream is stronger evidence of the
		// canonical remote than the conventional "origin", especially for forks.
		for _, branch := range []string{"main", "master"} {
			remote, _, ok, err := r.branchUpstream(ctx, logger, branch)
			if err != nil {
				upstreamEvidenceErr = errors.Join(upstreamEvidenceErr, fmt.Errorf("read upstream for default branch %q: %w", branch, err))
				continue
			}
			if ok && remote != "." && slices.Contains(r.Remotes, remote) {
				r.DefaultRemote = remote
				break
			}
		}
	}
	if r.DefaultRemote == "" {
		switch {
		case len(r.Remotes) == 1:
			r.DefaultRemote = r.Remotes[0]
		case slices.Contains(r.Remotes, "origin"):
			r.DefaultRemote = "origin"
		default:
			return errors.Join(fmt.Errorf("multiple remotes and no %q", "origin"), upstreamEvidenceErr)
		}
	}
	if len(r.Remotes) == 0 {
		// Repo labels created before Remotes was added only recorded the default.
		r.Remotes = []string{r.DefaultRemote}
	}
	// Legacy labels did not record all remotes. Recover the remotes needed by
	// mapped branches so triangular upstream/push configuration keeps working.
	for _, branch := range r.Branches {
		remote, _, ok, err := r.branchUpstream(ctx, logger, branch)
		if err != nil {
			// Mapped-branch validation reports malformed upstream metadata with
			// repository-default repair guidance after defaults are resolved.
			continue
		}
		if ok && remote != "." && !slices.Contains(r.Remotes, remote) && !r.isMDTransportRemote(ctx, g, remote) {
			r.Remotes = append(r.Remotes, remote)
		}
		pushRemote, err := r.branchPushRemote(ctx, logger, branch)
		if err != nil {
			return err
		}
		if pushRemote != "" && pushRemote != "." && !slices.Contains(r.Remotes, pushRemote) && !r.isMDTransportRemote(ctx, g, pushRemote) {
			r.Remotes = append(r.Remotes, pushRemote)
		}
	}
	if r.DefaultBranch == "" {
		branch, err := g.DefaultBranch(ctx, r.DefaultRemote)
		if err != nil {
			return err
		}
		r.DefaultBranch = branch
	}
	return nil
}

// populateContainerPath sets ContainerPath from GitRoot if not already set.
func (r *Repo) populateContainerPath() {
	if r.ContainerPath == "" {
		r.ContainerPath = "/home/user/src/" + strings.TrimSuffix(filepath.Base(r.GitRoot), ".git")
	}
}

// containerTagRefspecs returns push refspecs for host tags selected by
// TagRegexp. An empty TagRegexp selects no tags and avoids invoking Git.
func (r *Repo) containerTagRefspecs(ctx context.Context, logger *slog.Logger) ([]string, error) {
	if r.TagRegexp == "" {
		return nil, nil
	}
	out, err := (&git.Checkout{Root: r.GitRoot, Logger: logger}).RunGit(ctx, "tag", "--list")
	if err != nil {
		return nil, err
	}
	return tagRefspecs(r.TagRegexp, out)
}

func (r *Repo) containerSyncRefspecs(ctx context.Context, logger *slog.Logger) ([]string, error) {
	// Mirror every cached branch for the remotes that existed when the container
	// was created. This makes remote branches usable even when the container
	// cannot access the remote itself.
	if err := r.resolveDefaults(ctx, logger); err != nil {
		return nil, fmt.Errorf("sync remote branches: %w", err)
	}
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	refspecs := []string{}
	for _, remote := range r.Remotes {
		branches, err := g.ListBranches(ctx, remote)
		if err != nil {
			return nil, fmt.Errorf("list branches for remote %q: %w", remote, err)
		}
		for _, branch := range branches {
			ref := remoteTrackingRef(remote, branch[0])
			refspecs = appendUniqueRefspecs(refspecs, ref+":"+ref)
		}
	}
	for _, branch := range r.Branches {
		remote, upstreamBranch, err := r.requiredMappedBranchUpstream(ctx, logger, branch)
		if err != nil {
			return nil, err
		}
		if remote != "." || slices.Contains(r.Branches, upstreamBranch) {
			continue
		}
		upstreamRef := "refs/heads/" + upstreamBranch
		exists, err := g.RefExists(ctx, upstreamRef)
		if err != nil {
			return nil, fmt.Errorf("check local upstream for mapped branch %q: %w", branch, err)
		}
		if !exists {
			return nil, r.upstreamRepairError(ctx, g, branch, fmt.Sprintf("mapped host branch %q tracks missing local branch %q", branch, upstreamBranch))
		}
		refspecs = appendUniqueRefspecs(refspecs, upstreamRef+":"+upstreamRef)
	}
	tagRefspecs, err := r.containerTagRefspecs(ctx, logger)
	if err != nil {
		return nil, fmt.Errorf("select tags: %w", err)
	}
	refspecs = appendUniqueRefspecs(refspecs, tagRefspecs...)

	// A repository can record a default branch that exists only as a local
	// branch. Preserve the historical fallback for that case.
	src, ok, err := defaultBranchPushSource(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("sync default branch %q: %w", r.DefaultBranch, err)
	}
	if ok {
		refspecs = appendUniqueRefspecs(refspecs, src+":"+remoteTrackingRef(r.DefaultRemote, r.DefaultBranch))
	}
	return refspecs, nil
}

// resolveContainerBranchBase chooses the ref that should seed branch inside the
// container and the destination ref to push into the container remote.
//
// It prefers the branch's configured upstream when it matches the local branch,
// then the default remote's same-named tracking branch, and finally the local
// branch via refs/md/incoming when the local branch contains unpushed commits.
func (r *Repo) resolveContainerBranchBase(ctx context.Context, logger *slog.Logger, branch string) (containerBranchBase, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	localRef := "refs/heads/" + branch
	localCommit, err := g.RevParse(ctx, localRef)
	if err != nil {
		return containerBranchBase{}, err
	}
	pushRemote, err := r.branchPushRemote(ctx, logger, branch)
	if err != nil {
		return containerBranchBase{}, err
	}
	if r.isMDTransportRemote(ctx, g, pushRemote) {
		pushRemote = ""
	}
	remote, upstreamBranch, err := r.requiredMappedBranchUpstream(ctx, logger, branch)
	if err != nil {
		return containerBranchBase{}, err
	}
	base := containerBranchBase{
		branch:         branch,
		upstreamRemote: remote,
		upstreamBranch: upstreamBranch,
		pushRemote:     pushRemote,
	}
	if remote != "." {
		remoteRef := remoteTrackingRef(remote, upstreamBranch)
		if remoteCommit, err := g.RevParse(ctx, remoteRef); err == nil && remoteCommit == localCommit {
			base.source = remoteRef
			base.ref = remote + "/" + upstreamBranch
			base.destination = remoteRef
			return base, nil
		}
	}
	remoteRef := remoteTrackingRef(r.DefaultRemote, branch)
	if remoteCommit, err := g.RevParse(ctx, remoteRef); err == nil && remoteCommit == localCommit {
		base.source = remoteRef
		base.ref = r.DefaultRemote + "/" + branch
		base.destination = remoteRef
		return base, nil
	}
	base.source = localRef
	base.ref = containerIncomingRefPrefix + branch
	base.destination = containerIncomingRefPrefix + branch
	return base, nil
}

// branchUpstream returns branch's configured upstream remote and branch name.
//
// ok is false when branch has no upstream. An error is returned when git reports
// malformed upstream metadata.
func (r *Repo) branchUpstream(ctx context.Context, logger *slog.Logger, branch string) (remote, upstreamBranch string, ok bool, err error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	remote, err = optionalGitConfig(ctx, g, "branch."+branch+".remote")
	if err != nil {
		return "", "", false, err
	}
	remoteRef, err := optionalGitConfig(ctx, g, "branch."+branch+".merge")
	if err != nil {
		return "", "", false, err
	}
	if remote == "" && remoteRef == "" {
		return "", "", false, nil
	}
	if remote == "" || remoteRef == "" {
		return "", "", false, fmt.Errorf("incomplete upstream configuration for branch %q", branch)
	}
	upstreamBranch, ok = strings.CutPrefix(remoteRef, "refs/heads/")
	if !ok || upstreamBranch == "" {
		return "", "", false, fmt.Errorf("invalid upstream ref for branch %q: %q", branch, remoteRef)
	}
	return remote, upstreamBranch, true, nil
}

func (r *Repo) requiredMappedBranchUpstream(ctx context.Context, logger *slog.Logger, branch string) (remote, upstreamBranch string, err error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	exists, err := g.RefExists(ctx, "refs/heads/"+branch)
	if err != nil {
		return "", "", fmt.Errorf("checking mapped host branch %q: %w", branch, err)
	}
	if !exists {
		problem := fmt.Sprintf("mapped host branch %q no longer exists", branch)
		if command, reason, ok := r.defaultBranchCommand(ctx, g, branch); ok {
			return "", "", fmt.Errorf("%s; recreate it from the repository default with: %s", problem, command)
		} else {
			return "", "", fmt.Errorf("%s; cannot derive a safe start point because %s; choose a start point and recreate the branch before using md", problem, reason)
		}
	}
	remote, upstreamBranch, ok, err := r.branchUpstream(ctx, logger, branch)
	if err != nil {
		return "", "", r.upstreamRepairError(ctx, g, branch, err.Error())
	}
	if !ok {
		return "", "", r.upstreamRepairError(ctx, g, branch, fmt.Sprintf("mapped host branch %q has no upstream", branch))
	}
	if r.isMDTransportRemote(ctx, g, remote) {
		return "", "", r.upstreamRepairError(ctx, g, branch, fmt.Sprintf("mapped host branch %q tracks md container remote %q", branch, remote))
	}
	if len(r.Branches) > 1 && branch != r.Branches[0] && remote == "." && upstreamBranch == r.Branches[0] {
		return "", "", r.upstreamRepairError(ctx, g, branch, fmt.Sprintf("mapped host branch %q tracks primary branch %q locally", branch, r.Branches[0]))
	}
	return remote, upstreamBranch, nil
}

func (r *Repo) usableDefaultUpstream(ctx context.Context, g *git.Checkout) (upstream, reason string, ok bool) {
	if r.DefaultRemote == "" || r.DefaultBranch == "" {
		return "", "the repository default is incomplete", false
	}
	remotes, err := g.Remotes(ctx)
	if err != nil {
		return "", fmt.Sprintf("configured remotes could not be read: %v", err), false
	}
	if !slices.Contains(remotes, r.DefaultRemote) {
		return "", fmt.Sprintf("the recorded default remote %q is not configured", r.DefaultRemote), false
	}
	upstream = r.DefaultRemote + "/" + r.DefaultBranch
	exists, err := g.RefExists(ctx, remoteTrackingRef(r.DefaultRemote, r.DefaultBranch))
	if err != nil {
		return "", fmt.Sprintf("the default remote-tracking branch %q could not be checked: %v", upstream, err), false
	}
	if !exists {
		return "", fmt.Sprintf("the default remote-tracking branch %q does not exist", upstream), false
	}
	return upstream, "", true
}

func (r *Repo) defaultBranchCommand(ctx context.Context, g *git.Checkout, branch string) (command, reason string, ok bool) {
	upstream, reason, ok := r.usableDefaultUpstream(ctx, g)
	if !ok {
		return "", reason, false
	}
	command = shellQuoteArgs([]string{"git", "-C", r.GitRoot, "branch", "--track", branch, upstream})
	return command, "", true
}

func (r *Repo) upstreamRepairError(ctx context.Context, g *git.Checkout, branch, problem string) error {
	upstream, reason, ok := r.usableDefaultUpstream(ctx, g)
	if !ok {
		return fmt.Errorf("%s; cannot derive a safe upstream because %s; choose a remote branch and configure it as the upstream before using md", problem, reason)
	}
	command := shellQuoteArgs([]string{"git", "-C", r.GitRoot, "branch", "--set-upstream-to=" + upstream, branch})
	return fmt.Errorf("%s; to track the repository default %q instead, run: %s", problem, upstream, command)
}

func (r *Repo) validateMappedBranchUpstreams(ctx context.Context, logger *slog.Logger) error {
	for _, branch := range r.Branches {
		if _, _, err := r.requiredMappedBranchUpstream(ctx, logger, branch); err != nil {
			return err
		}
	}
	return nil
}

type branchUpstreamSpec struct {
	remote     string
	branch     string
	pushRemote string
}

func (r *Repo) mappedBranchUpstreamSpec(ctx context.Context, logger *slog.Logger, branch string) (branchUpstreamSpec, error) {
	remote, upstreamBranch, err := r.requiredMappedBranchUpstream(ctx, logger, branch)
	if err != nil {
		return branchUpstreamSpec{}, err
	}
	pushRemote, err := r.branchPushRemote(ctx, logger, branch)
	if err != nil {
		return branchUpstreamSpec{}, fmt.Errorf("reading push remote for mapped branch %q: %w", branch, err)
	}
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	if r.isMDTransportRemote(ctx, g, pushRemote) {
		pushRemote = ""
	}
	return branchUpstreamSpec{remote: remote, branch: upstreamBranch, pushRemote: pushRemote}, nil
}

func (r *Repo) containerBranchUpstreamCommands(ctx context.Context, logger *slog.Logger) ([]string, error) {
	commands := make([]string, 0, 4*len(r.Branches))
	for _, branch := range r.Branches {
		spec, err := r.mappedBranchUpstreamSpec(ctx, logger, branch)
		if err != nil {
			return nil, err
		}
		commands = append(commands, branchTrackingCommands(branch, spec)...)
	}
	return commands, nil
}

// branchPushRemote returns the remote Git would use to push branch.
func (r *Repo) branchPushRemote(ctx context.Context, logger *slog.Logger, branch string) (string, error) {
	out, err := (&git.Checkout{Root: r.GitRoot, Logger: logger}).RunGit(ctx, "for-each-ref", "--format=%(push:remotename)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (r *Repo) resolveForkExtraBranchBase(ctx context.Context, logger *slog.Logger, sourceBranch, destinationBranch string) (containerBranchBase, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	pushRemote, err := r.branchPushRemote(ctx, logger, sourceBranch)
	if err != nil {
		return containerBranchBase{}, fmt.Errorf("reading push remote: %w", err)
	}
	if r.isMDTransportRemote(ctx, g, pushRemote) {
		pushRemote = ""
	}
	remote, upstreamBranch, err := r.requiredMappedBranchUpstream(ctx, logger, sourceBranch)
	if err != nil {
		return containerBranchBase{}, fmt.Errorf("reading upstream: %w", err)
	}
	return containerBranchBase{
		branch:         destinationBranch,
		ref:            containerIncomingRefPrefix + destinationBranch,
		destination:    containerIncomingRefPrefix + destinationBranch,
		upstreamRemote: remote,
		upstreamBranch: upstreamBranch,
		pushRemote:     pushRemote,
	}, nil
}

func (r *Repo) containerRemoteConfigs(ctx context.Context, logger *slog.Logger) ([]containerRemoteConfig, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	out, err := g.RunGit(ctx, "remote")
	if err != nil {
		return nil, fmt.Errorf("list remotes: %w", err)
	}
	configured := make(map[string]struct{})
	for remote := range strings.SplitSeq(out, "\n") {
		if remote != "" {
			configured[remote] = struct{}{}
		}
	}
	configs := make([]containerRemoteConfig, 0, len(r.Remotes))
	for _, remote := range r.Remotes {
		config := containerRemoteConfig{name: remote}
		if _, ok := configured[remote]; ok {
			config.configured = true
			fetchURL, err := g.RunGit(ctx, "remote", "get-url", remote)
			if err != nil {
				return nil, fmt.Errorf("read URL for remote %q: %w", remote, err)
			}
			pushURL, err := g.RunGit(ctx, "remote", "get-url", "--push", remote)
			if err != nil {
				return nil, fmt.Errorf("read push URL for remote %q: %w", remote, err)
			}
			config.url = convertGitURLToHTTPS(fetchURL)
			config.pushURL = convertGitURLToHTTPS(pushURL)
		}
		configs = append(configs, config)
	}
	return configs, nil
}

func (r *Repo) gitIdentity(ctx context.Context, logger *slog.Logger) (gitIdentity, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	name, err := optionalGitConfig(ctx, g, "user.name")
	if err != nil {
		return gitIdentity{}, fmt.Errorf("read Git user name for %s: %w", r.GitRoot, err)
	}
	email, err := optionalGitConfig(ctx, g, "user.email")
	if err != nil {
		return gitIdentity{}, fmt.Errorf("read Git user email for %s: %w", r.GitRoot, err)
	}
	return gitIdentity{name: name, email: email}, nil
}

func (r *Repo) containerHooksPath(ctx context.Context, logger *slog.Logger) (string, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	hooksPath, err := g.RunGit(ctx, "config", "--local", "--get", "core.hooksPath")
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("read core.hooksPath for %s: %w", r.GitRoot, err)
	}
	if hooksPath == "" || filepath.IsAbs(hooksPath) || strings.HasPrefix(hooksPath, "~") {
		return "", nil
	}
	repoRoot, err := filepath.Abs(r.GitRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root %s: %w", r.GitRoot, err)
	}
	resolvedPath, err := filepath.Abs(filepath.Join(repoRoot, hooksPath))
	if err != nil {
		return "", fmt.Errorf("resolve core.hooksPath %q: %w", hooksPath, err)
	}
	relativePath, err := filepath.Rel(repoRoot, resolvedPath)
	if err != nil {
		return "", fmt.Errorf("relativize core.hooksPath %q: %w", hooksPath, err)
	}
	if relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", nil
	}
	return filepath.ToSlash(hooksPath), nil
}

func optionalGitConfig(ctx context.Context, g *git.Checkout, key string) (string, error) {
	value, err := g.RunGit(ctx, "config", "--get", key)
	if err == nil {
		return value, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return "", nil
	}
	return "", err
}

// createForkHostBranch creates destinationBranch at startPoint and gives it the
// source branch's upstream. It returns enough prior state to roll the change
// back safely if the surrounding fork operation fails.
func (r *Repo) createForkHostBranch(ctx context.Context, logger *slog.Logger, destinationBranch, startPoint string, upstream branchUpstreamSpec) (forkHostBranchChange, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	change := forkHostBranchChange{
		gitRoot: r.GitRoot,
		branch:  destinationBranch,
		written: forkHostBranchConfig{
			remote: []string{upstream.remote},
			merge:  []string{"refs/heads/" + upstream.branch},
		},
	}
	if upstream.pushRemote != "" {
		change.written.pushRemote = []string{upstream.pushRemote}
	}
	var err error
	change.writtenOID, err = g.RevParse(ctx, startPoint)
	if err != nil {
		return change, fmt.Errorf("resolving fork branch start point %q: %w", startPoint, err)
	}
	change.previouslySet, err = g.RefExists(ctx, "refs/heads/"+destinationBranch)
	if err != nil {
		return change, fmt.Errorf("checking fork branch %q: %w", destinationBranch, err)
	}
	if change.previouslySet {
		change.previousOID, err = g.RevParse(ctx, "refs/heads/"+destinationBranch)
		if err != nil {
			return change, fmt.Errorf("reading fork branch %q: %w", destinationBranch, err)
		}
	}
	change.previous, err = readForkHostBranchConfig(ctx, g, destinationBranch)
	if err != nil {
		return change, err
	}
	if _, err := g.RunGit(ctx, "branch", "--no-track", "-f", destinationBranch, startPoint); err != nil {
		return change, fmt.Errorf("creating fork branch %q: %w", destinationBranch, err)
	}
	change.refWritten = true
	if err := writeForkHostBranchConfig(ctx, g, destinationBranch, change.written); err != nil {
		return change, err
	}
	return change, nil
}

// forkHostBranchConfig preserves every value in the three Git configuration
// keys that define a host branch's fetch and push upstreams.
type forkHostBranchConfig struct {
	remote     []string
	merge      []string
	pushRemote []string
}

// forkHostBranchChange records a host branch before and after Fork mutates it.
// rollback uses both states to avoid overwriting a concurrent user change.
type forkHostBranchChange struct {
	gitRoot       string
	branch        string
	previousOID   string
	writtenOID    string
	previous      forkHostBranchConfig
	written       forkHostBranchConfig
	previouslySet bool
	refWritten    bool
}

func (c *forkHostBranchChange) rollback(ctx context.Context, logger *slog.Logger) error {
	if !c.refWritten {
		return nil
	}
	g := &git.Checkout{Root: c.gitRoot, Logger: logger}
	currentOID, err := g.RevParse(ctx, "refs/heads/"+c.branch)
	if err != nil {
		return fmt.Errorf("not rolling back host branch %q in %s because it changed after fork wrote it: %w", c.branch, c.gitRoot, err)
	}
	if currentOID != c.writtenOID {
		return fmt.Errorf("not rolling back host branch %q in %s because it changed after fork wrote it: now %s, expected %s", c.branch, c.gitRoot, currentOID, c.writtenOID)
	}
	currentConfig, err := readForkHostBranchConfig(ctx, g, c.branch)
	if err != nil {
		return err
	}
	entries := []struct {
		key      string
		current  []string
		previous []string
		written  []string
	}{
		{key: "merge", current: currentConfig.merge, previous: c.previous.merge, written: c.written.merge},
		{key: "pushRemote", current: currentConfig.pushRemote, previous: c.previous.pushRemote, written: c.written.pushRemote},
		{key: "remote", current: currentConfig.remote, previous: c.previous.remote, written: c.written.remote},
	}
	for _, entry := range entries {
		if !slices.Equal(entry.current, entry.previous) && !slices.Equal(entry.current, entry.written) {
			return fmt.Errorf("not rolling back host branch %q in %s because its %s configuration changed after fork wrote it", c.branch, c.gitRoot, entry.key)
		}
	}
	ref := "refs/heads/" + c.branch
	if c.previouslySet {
		if _, err := g.RunGit(ctx, "update-ref", ref, c.previousOID, c.writtenOID); err != nil {
			return fmt.Errorf("restoring host branch %q in %s: %w", c.branch, c.gitRoot, err)
		}
	} else if _, err := g.RunGit(ctx, "update-ref", "-d", ref, c.writtenOID); err != nil {
		return fmt.Errorf("deleting host branch %q in %s: %w", c.branch, c.gitRoot, err)
	}
	if err := writeForkHostBranchConfig(ctx, g, c.branch, c.previous); err != nil {
		return fmt.Errorf("restoring configuration for host branch %q in %s: %w", c.branch, c.gitRoot, err)
	}
	return nil
}

func readForkHostBranchConfig(ctx context.Context, g *git.Checkout, branch string) (forkHostBranchConfig, error) {
	values := func(key string) ([]string, error) {
		out, err := g.RunGit(ctx, "config", "--get-all", key)
		if err == nil {
			return strings.Split(out, "\n"), nil
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	prefix := "branch." + branch + "."
	remote, err := values(prefix + "remote")
	if err != nil {
		return forkHostBranchConfig{}, fmt.Errorf("reading remote for fork branch %q: %w", branch, err)
	}
	merge, err := values(prefix + "merge")
	if err != nil {
		return forkHostBranchConfig{}, fmt.Errorf("reading merge ref for fork branch %q: %w", branch, err)
	}
	pushRemote, err := values(prefix + "pushRemote")
	if err != nil {
		return forkHostBranchConfig{}, fmt.Errorf("reading push remote for fork branch %q: %w", branch, err)
	}
	return forkHostBranchConfig{remote: remote, merge: merge, pushRemote: pushRemote}, nil
}

func writeForkHostBranchConfig(ctx context.Context, g *git.Checkout, branch string, config forkHostBranchConfig) error {
	entries := []struct {
		key    string
		values []string
	}{
		{key: "merge", values: config.merge},
		{key: "pushRemote", values: config.pushRemote},
		{key: "remote", values: config.remote},
	}
	for _, entry := range entries {
		fullKey := "branch." + branch + "." + entry.key
		_, err := g.RunGit(ctx, "config", "--get-all", fullKey)
		if err == nil {
			if _, err := g.RunGit(ctx, "config", "--unset-all", fullKey); err != nil {
				return fmt.Errorf("clearing %s for fork branch %q: %w", entry.key, branch, err)
			}
		} else if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
			return fmt.Errorf("checking %s for fork branch %q: %w", entry.key, branch, err)
		}
		for _, value := range entry.values {
			if _, err := g.RunGit(ctx, "config", "--add", fullKey, value); err != nil {
				return fmt.Errorf("setting %s for fork branch %q: %w", entry.key, branch, err)
			}
		}
	}
	return nil
}

// StartOpts configures container startup.
type StartOpts struct {
	// BaseImage is the full Docker image reference (e.g.
	// "ghcr.io/caic-xyz/md-user:v0.7.1" or "myregistry/custom:tag"). When empty,
	// DefaultBaseImage is used.
	BaseImage string
	// Platform is the Linux container platform, e.g. "linux/amd64" or
	// "linux/arm64". Empty means use the host's native platform.
	Platform string
	// Display enables X11/VNC virtual display (port 5901).
	Display bool
	// Tailscale enables Tailscale networking inside the container.
	//
	// It is recommended to set Client.TailscaleAPIKey to enable ephemeral nodes. If Client.TailscaleAPIKey is
	// not set, the node will not be ephemeral. Instead, an authentication URL will be printed back by md.
	Tailscale bool
	// TailscaleAuthKey is a pre-authorized Tailscale auth key.
	//
	// When empty and Tailscale is true, Client.TailscaleAPIKey is used to generate an authentication key.
	//
	// The tailnet policy must allow "tag:md".
	//
	// https://tailscale.com/docs/features/access-control/auth-keys
	TailscaleAuthKey string
	// USB enables USB device passthrough (Linux only).
	USB bool
	// Sudo enables password-based sudo for the user account. A random
	// password is generated per container and stored as a container label;
	// retrieve it with `md sudo-password`. Defaults to false.
	Sudo bool
	// Caches lists host directories to COPY into the image at build time.
	// Use well-known names from [WellKnownCaches] or construct [CacheMount]
	// values directly. Paths that do not exist on the host are silently skipped.
	Caches []CacheMount
	// Mounts lists host directories to bind-mount into the running container.
	// Missing host directories are rejected before the runtime is invoked.
	Mounts []Mount
	// Labels are additional Docker labels (key=value) applied to the container.
	Labels []string
	// Quiet suppresses informational output during startup.
	Quiet bool
	// ExtraEnv holds environment updates to inject into the container's ~/.env.
	// Entries use KEY=VALUE to set a value or KEY= to remove one. Values are
	// shell-quoted before writing, so they may contain spaces and newlines.
	ExtraEnv []string
	// MaxCPUs limits the number of CPU cores the container may use.
	// Passed as --cpus to docker/podman. Zero means no limit.
	// Use [DefaultMaxCPUs] for a sensible default.
	MaxCPUs int
	// ExtraRunArgs are additional arguments passed verbatim to the
	// container runtime's "run" command. Not portable across runtimes.
	ExtraRunArgs []string

	// resetTailscale removes any Tailscale state inherited from a source image
	// before tailscaled starts. It is used for forked containers created from a
	// committed filesystem snapshot.
	resetTailscale bool
}

// StartResult contains Tailscale information from Connect. Port information
// is available on Container directly (SSHPort, VNCPort) after Launch returns.
type StartResult struct {
	// TailscaleFQDN is the Tailscale FQDN assigned to the container, if any.
	TailscaleFQDN string
	// TailscaleAuthURL is the Tailscale auth URL when no pre-auth key was provided.
	TailscaleAuthURL string
}

// ForkRepo describes one repository in a fork and the branches it carries.
type ForkRepo struct {
	// GitRoot is the absolute path of the host git repository. Fork matches it
	// against the source container's repos: a match is re-branched from the
	// snapshot, a non-match is initialized in the fork and pushed from the host.
	GitRoot string
	// SourceBranches are the branches to carry, primary first. For a repo present
	// in the source container it must equal the branches recorded on that repo
	// (Fork validates this). For a new repo it is the host branches to push; if
	// empty, it defaults to the repo's default upstream branch.
	SourceBranches []string
	// ContainerPath is the absolute container path to mount a new repo at. It is
	// used only for repos not already in the source container (existing repos
	// keep the snapshot's mount path). When empty, it defaults to
	// /home/user/src/<basename>.
	ContainerPath string
	// TagRegexp selects tags for a new repo. Existing source repos retain their
	// persisted tag selection.
	TagRegexp string
	// DestPrimary is the fork's primary branch, created from SourceBranches[0].
	// It must not be one of the repo's mapped branches, since Fork force-resets
	// the destination host branch. Fork does not check it against other
	// containers' branches or unrelated local refs, so the caller still owns
	// uniqueness there. Additional (non-primary) branches keep their source
	// names.
	DestPrimary string
}

// ForkOpts configures a container fork operation.
type ForkOpts struct {
	// Repos is the full set of repositories the fork should contain. It must
	// include an entry for every repo in the source container (identified by
	// GitRoot); additional entries are new repos initialized in the fork and
	// pushed from the host. Fork fails if a source-container repo is missing, if
	// GitRoots are duplicated, or if a DestPrimary is empty.
	Repos []ForkRepo
	// Display enables X11/VNC virtual display on the forked container.
	Display bool
	// Tailscale enables Tailscale networking on the forked container.
	Tailscale bool
	// USB enables USB device passthrough on the forked container.
	USB bool
	// Sudo enables root access via sudo on the forked container.
	Sudo bool
	// Labels are additional Docker labels (key=value) applied to the forked container.
	Labels []string
	// Quiet suppresses informational output.
	Quiet bool
	// ExtraEnv holds environment updates to inject into the container's ~/.env.
	// Entries use KEY=VALUE to set a value or KEY= to remove one. Values are
	// shell-quoted before writing, so they may contain spaces and newlines.
	ExtraEnv []string
	// Mounts lists host directories to bind-mount into the running container.
	// Missing host directories are rejected before the runtime is invoked.
	Mounts []Mount
	// MaxCPUs limits the number of CPU cores the forked container may use.
	// Passed as --cpus to docker/podman. Zero means no limit.
	// Use [DefaultMaxCPUs] for a sensible default.
	MaxCPUs int
	// ExtraRunArgs are additional arguments passed verbatim to the
	// container runtime's "run" command. Not portable across runtimes.
	ExtraRunArgs []string
}

// startOptions converts fork options into startup options for the forked container.
func (opts *ForkOpts) startOptions() *StartOpts {
	startOpts := &StartOpts{
		Quiet:        opts.Quiet,
		Labels:       opts.Labels,
		ExtraEnv:     opts.ExtraEnv,
		Mounts:       opts.Mounts,
		Display:      opts.Display,
		Tailscale:    opts.Tailscale,
		USB:          opts.USB,
		Sudo:         opts.Sudo,
		MaxCPUs:      opts.MaxCPUs,
		ExtraRunArgs: append(opts.runtimeEnvOverrides(), opts.ExtraRunArgs...),
	}
	startOpts.resetTailscale = startOpts.Tailscale
	return startOpts
}

// runtimeEnvOverrides returns run arguments that clear inherited startup env.
//
// Fork snapshots can contain ENV values from the source container image. Empty
// assignments force disabled fork capabilities to stay disabled at startup.
func (opts *ForkOpts) runtimeEnvOverrides() []string {
	var args []string
	if !opts.Display {
		args = append(args, "-e", "MD_DISPLAY=")
	}
	if !opts.Tailscale {
		args = append(args,
			"-e", "MD_TAILSCALE=",
			"-e", "MD_TAILSCALE_EPHEMERAL=",
			"-e", "MD_TAILSCALE_RESET=",
			"-e", "TAILSCALE_AUTHKEY=",
		)
	}
	if !opts.Sudo {
		args = append(args, "-e", "MD_SUDO_PASSWORD=")
	}
	return args
}

// FetchOpts configures [Container.Fetch]. Its zero value fetches only what the
// container already committed.
type FetchOpts struct {
	// Provider generates the commit message when Commit is set. A nil Provider
	// uses a fixed message.
	Provider genai.Provider
	// ContextTokens is the selected model's context window. Zero uses the
	// commit-message generator default.
	ContextTokens int
	// Commit stages and commits the container's pending changes before
	// fetching, so the host receives them as well. Leave it unset to observe
	// the container without changing its history or its working tree.
	Commit bool
	// NoVerify passes --no-verify to the container commit, skipping the
	// pre-commit and commit-msg hooks. It has no effect unless Commit is set.
	NoVerify bool
}

// PullOpts configures [Container.Pull].
type PullOpts struct {
	// Provider generates the commit message. A nil Provider uses a fixed message.
	Provider genai.Provider
	// ContextTokens is the selected model's context window. Zero uses the
	// commit-message generator default.
	ContextTokens int
	// NoVerify passes --no-verify to the container commit, skipping the
	// pre-commit and commit-msg hooks.
	NoVerify bool
}

// FetchedBranch identifies an exact container branch tip observed by
// [Container.Fetch].
type FetchedBranch struct {
	// BranchName is the short local branch name in the container, such as
	// "main" or "caic-1".
	BranchName string
	// CommitHash is the full Git object ID of the fetched branch tip.
	CommitHash string
}

// DiffOpts configures [Container.Diff]. Its zero value reports container work
// not yet integrated into the host branch.
type DiffOpts struct {
	// Args are extra arguments forwarded to git diff, for example "--stat".
	Args []string
	// Full reports the whole branch, from the merge base with its upstream,
	// instead of only the work not yet integrated into the host branch.
	Full bool
}

// Container holds state for a single container instance.
//
// Fields marked with a label are persisted as Docker container labels
// and restored by [unmarshalContainer] when listing containers.
type Container struct {
	// Client is the client that created this container.
	*Client

	// Logger receives logs for this container. It includes the cntr attribute.
	Logger *slog.Logger
	// Repos are the git repositories in this container. Repos[0] is the
	// primary; the rest are pushed alongside it. Each repo's ContainerPath
	// gives the absolute destination path.
	// Label: md.repos (base64-encoded JSON)
	Repos []Repo
	// Name is the Docker container name (e.g. "md-myrepo-main").
	Name string
	// State is the Docker container state (e.g. "running", "exited").
	State string
	// CreatedAt is when the container was created.
	CreatedAt time.Time
	// Labels contains all Docker/Podman labels observed on the container.
	Labels map[string]string
	// Display indicates the container was started with X11/VNC enabled.
	// Label: md.display
	Display bool
	// Tailscale indicates the container was started with Tailscale networking.
	// Label: md.tailscale
	Tailscale bool
	// USB indicates the container was started with USB passthrough.
	// Label: md.usb
	USB bool
	// Sudo indicates root access via sudo is enabled.
	// Label: md.sudo
	Sudo bool
	// SSHPort is the host port mapped to the container's SSH port.
	// Set by Launch; available immediately after Launch returns.
	SSHPort int32
	// VNCPort is the host port mapped to the container's VNC port, if display is enabled.
	// Set by Launch; available immediately after Launch returns. Zero if display is disabled.
	VNCPort int32

	// sshConfigPath is the SSH config file for this container (~/.ssh/config.d/<name>.conf).
	// Set by Launch and Revive. Used by SSHCommand to pass -F directly.
	sshConfigPath string
	// sudoPassword is the random password set by Launch, cached
	// so SudoPassword() doesn't need to docker inspect. Empty for
	// containers loaded from docker list (fall back to label).
	sudoPassword string
	// tailscaleEphemeral is set by Launch and consumed by Connect.
	tailscaleEphemeral bool
}

// SSHCommand returns SSH command args for this container.
// opts are SSH flags (e.g. "-q", "-t"); cmd is the remote command.
// The container name is always included as the SSH host target.
// If cmd is empty, only the base SSH args and host are returned (for interactive sessions).
func (c *Container) SSHCommand(opts []string, cmd string) []string {
	args := []string{"ssh"}
	if c.sshConfigPath != "" {
		args = append(args, "-F", c.sshConfigPath)
	}
	args = append(args, opts...)
	args = append(args, c.Name)
	if cmd != "" {
		args = append(args, cmd)
	}
	return args
}

// resolveContainerPaths sets ContainerPath for any repo that doesn't have one,
// using filepath.Base(GitRoot) by default. When any two repos share the
// same basename, all auto-populated repos switch to paths relative to the
// common parent directory of their GitRoots.
func resolveContainerPaths(repos []Repo) error {
	// Track which repos were auto-populated (no explicit ContainerPath).
	auto := make([]bool, len(repos))
	for i := range repos {
		auto[i] = repos[i].ContainerPath == ""
		repos[i].populateContainerPath()
	}

	// Detect basename conflicts.
	seen := make(map[string]struct{}, len(repos))
	hasConflict := false
	for i := range repos {
		if _, dup := seen[repos[i].ContainerPath]; dup {
			hasConflict = true
			break
		}
		seen[repos[i].ContainerPath] = struct{}{}
	}
	if !hasConflict {
		return nil
	}

	// Conflict detected: switch all auto-populated repos to relative paths
	// from their common parent directory.
	var autoDirs []string
	for i := range repos {
		if auto[i] {
			autoDirs = append(autoDirs, repos[i].GitRoot)
		}
	}
	base := commonParent(autoDirs)
	for i := range repos {
		if !auto[i] {
			continue
		}
		rel, err := filepath.Rel(base, repos[i].GitRoot)
		if err != nil {
			return fmt.Errorf("repos[%d]: cannot compute relative path from %q: %w", i, base, err)
		}
		repos[i].ContainerPath = "/home/user/src/" + filepath.ToSlash(rel)
	}

	// Final validation: check for remaining duplicate mount paths.
	final := make(map[string]int, len(repos))
	for i := range repos {
		if j, dup := final[repos[i].ContainerPath]; dup {
			return fmt.Errorf("repos[%d] and repos[%d] both mount as %q; set ContainerPath to disambiguate", j, i, repos[i].ContainerPath)
		}
		final[repos[i].ContainerPath] = i
	}
	return nil
}

// commonParent returns the longest common path prefix across all dirs.
// Returns "/" if there is no common prefix beyond the root.
func commonParent(dirs []string) string {
	if len(dirs) == 0 {
		return "/"
	}
	common := dirs[0]
	for _, d := range dirs[1:] {
		common = commonPrefix(common, d)
		if common == "/" {
			break
		}
	}
	return common
}

// commonPrefix returns the longest common directory prefix of two paths.
func commonPrefix(a, b string) string {
	minLen := min(len(a), len(b))
	i := 0
	for i < minLen && a[i] == b[i] {
		i++
	}
	// Back up to the last complete path separator.
	for i > 0 && a[i-1] != '/' {
		i--
	}
	if i == 0 {
		return "/"
	}
	return a[:i]
}

// checkRepoOverlap reports an error if repos overlaps, on any GitRoot and
// branch, with the repos of a container in existing other than name.
func checkRepoOverlap(name string, repos []Repo, existing []*Container) error {
	for _, other := range existing {
		if other.Name == name {
			continue
		}
		for i := range repos {
			r := &repos[i]
			for j := range other.Repos {
				or := &other.Repos[j]
				if r.GitRoot != or.GitRoot {
					continue
				}
				for _, b := range r.Branches {
					if slices.Contains(or.Branches, b) {
						return fmt.Errorf("repo %s branch %s is already mapped into container %s; commands that locate a container by repo+branch would become ambiguous. Stop that container first, or pick a different branch", r.GitRoot, b, other.Name)
					}
				}
			}
		}
	}
	return nil
}

// Launch prepares the image and starts the Docker container. It does NOT
// wait for SSH to become ready — call Connect to complete startup once the
// container's repos have their branches set (e.g. after concurrent branch
// allocation).
func (c *Container) Launch(ctx context.Context, stdout, stderr io.Writer, opts *StartOpts) (retErr error) {
	// Resolve mount paths, disambiguating repos with the same basename
	// using relative paths. After this, all MountedPaths are unique.
	if err := resolveContainerPaths(c.Repos); err != nil {
		return err
	}
	for i := range c.Repos {
		if len(c.Repos[i].Remotes) == 0 {
			if err := c.Repos[i].resolveRemotes(ctx, c.Logger, ""); err != nil {
				return fmt.Errorf("resolve remotes for %s: %w", c.Repos[i].ContainerPath, err)
			}
		}
		if err := c.Repos[i].resolveDefaults(ctx, c.Logger); err != nil {
			return fmt.Errorf("resolve defaults for %s: %w", c.Repos[i].ContainerPath, err)
		}
		if err := c.Repos[i].validateMappedBranchUpstreams(ctx, c.Logger); err != nil {
			return fmt.Errorf("validate mapped branches for %s: %w", c.Repos[i].GitRoot, err)
		}
	}

	// Check if container already exists. Container names include both
	// repo and branch, so collisions are rare (same repo+branch launched
	// twice, or two repos with the same basename from different
	// directories on the same branch). Append a short random hex suffix
	// (4 bytes) as a safe fallback.
	//
	// 4 hex bytes = 65K namespaces, negligible collision probability.
	if _, err := c.Runtime.Run(ctx, "", "inspect", c.Name); err == nil {
		var suffix [4]byte
		_, _ = rand.Read(suffix[:])
		c.Name = c.Name + "-" + hex.EncodeToString(suffix[:])
		if _, err := c.Runtime.Run(ctx, "", "inspect", c.Name); err == nil {
			return fmt.Errorf("container %s already exists. SSH in with 'ssh %s' or clean it up via 'md purge' first",
				c.Name, c.Name)
		}
	}

	// A repo+branch can be the *primary* of one container and an *extra*
	// repo of another (they get different names, so the check above misses
	// it), leaving two containers mapping the same repo+branch. That is an
	// unresolvable ambiguity for anything that locates a container by
	// repo+branch (e.g. `md diff`): neither -branch nor any other flag can
	// disambiguate two containers that genuinely hold the same branch.
	existing, err := c.List(ctx)
	if err != nil {
		return err
	}
	if err := checkRepoOverlap(c.Name, c.Repos, existing); err != nil {
		return err
	}

	baseImage := opts.BaseImage
	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	imageName, err := c.ensureImage(ctx, stdout, stderr, baseImage, opts.Platform, opts.Caches, opts.Quiet)
	if err != nil {
		return err
	}
	c.prepareTailscaleAuthKey(ctx, stdout, opts)
	c.Display = opts.Display
	c.USB = opts.USB
	c.Sudo = opts.Sudo
	return c.launchContainer(ctx, stdout, stderr, opts, imageName)
}

// Connect waits for SSH, pushes repos into the container, and completes
// startup. Must be called after Launch. Container.Repos must have
// branches set before this call.
func (c *Container) Connect(ctx context.Context, stdout, stderr io.Writer, opts *StartOpts) (*StartResult, error) {
	result, err := c.provisionContainer(ctx, stdout, stderr, opts)
	if err != nil {
		return nil, err
	}
	if opts.Tailscale {
		c.Tailscale = true
		c.State = "running"
		result.TailscaleFQDN = c.TailscaleFQDN(ctx)
	}
	return result, nil
}

// Revive restarts a stopped (exited) container. It validates git remotes,
// runs `docker start`, re-queries the SSH port (which changes on restart),
// rewrites the SSH config, and waits for SSH to become ready. It does NOT
// push repos or send .env — the container's filesystem is preserved across
// stop/start.
func (c *Container) Revive(ctx context.Context, stdout, stderr io.Writer) error {
	for i := range c.Repos {
		if err := c.Repos[i].validateMappedBranchUpstreams(ctx, c.Logger); err != nil {
			return fmt.Errorf("validate mapped branches for %s: %w", c.Repos[i].GitRoot, err)
		}
	}

	// Validate git remotes before starting. Each remote must either be
	// absent (will be added) or point to the expected URL. A remote
	// pointing elsewhere indicates a name collision — fail early.
	for i := range c.Repos {
		r := &c.Repos[i]
		rPath := r.ContainerPath
		wantURL := "user@" + c.Name + ":" + rPath
		got, err := c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "get-url", c.Name})
		if err == nil {
			if got != wantURL {
				return fmt.Errorf("git remote %s in %s points to %q, expected %q", c.Name, r.GitRoot, got, wantURL)
			}
			// Remote exists and is correct — nothing to do.
			continue
		}
		// Remote doesn't exist, add it.
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "remote", "add", c.Name, wantURL}, stdout, stderr); err != nil {
			return fmt.Errorf("adding git remote for %s: %w", rPath, err)
		}
	}

	// Start the stopped container.
	if _, err := c.Runtime.Run(ctx, "", "start", c.Name); err != nil {
		return fmt.Errorf("docker start %s: %w", c.Name, err)
	}

	// Query the new port mappings in one inspect call. They change on restart.
	if err := c.refreshRuntimeFields(ctx); err != nil {
		return fmt.Errorf("inspecting container after revive: %w", err)
	}
	port := c.SSHPort
	if port == 0 {
		return fmt.Errorf("container %s has no SSH port mapping after revive", c.Name)
	}

	// Rewrite SSH config with the new port. The known_hosts file also
	// needs rewriting because entries are keyed by [127.0.0.1]:port.
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	if err := removeSSHConfig(ctx, c.Client, sshConfigDir, c.Name); err != nil {
		return err
	}
	c.sshConfigPath = filepath.Join(sshConfigDir, c.Name+".conf")
	knownHostsPath := filepath.Join(sshConfigDir, c.Name+".known_hosts")
	hostPubKey, err := os.ReadFile(c.HostKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("reading host public key: %w", err)
	}
	if err := writeSSHConfig(sshConfigDir, c.Name, port, c.UserKeyPath, knownHostsPath, c.ControlMaster); err != nil {
		return fmt.Errorf("writing SSH config: %w", err)
	}
	if err := writeKnownHosts(knownHostsPath, port, strings.TrimSpace(string(hostPubKey))); err != nil {
		return fmt.Errorf("writing known_hosts: %w", err)
	}

	// Wait for TCP, then confirm SSH is fully ready.
	addr := fmt.Sprintf("localhost:%d", port)
	if err := waitForTCP(ctx, addr, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return fmt.Errorf("waiting for SSH port on %s: %w", c.Name, err)
	}
	if err := c.waitForSSH(ctx, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return fmt.Errorf("SSH handshake on %s: %w", c.Name, err)
	}
	if err := c.configureGlobalGitIdentity(ctx, stdout, stderr); err != nil {
		return err
	}

	// Refresh cached remote refs and remote configuration without resetting the
	// preserved working branches.
	for i := range c.Repos {
		if err := c.SyncDefaultBranch(ctx, i); err != nil {
			return fmt.Errorf("syncing remote branches after revive: %w", err)
		}
		if err := c.configureContainerRemotes(ctx, stdout, stderr, i, true); err != nil {
			return err
		}
	}

	c.State = "running"
	return nil
}

// Stop stops the container without removing it. The container can be
// restarted later with Revive. SSH config is preserved (Revive rewrites
// it with the new port), but the ControlMaster socket is removed to
// prevent stale connections from interfering with subsequent SSH commands.
func (c *Container) Stop(ctx context.Context) error {
	if _, err := c.Runtime.Run(ctx, "", "stop", c.Name); err != nil {
		return fmt.Errorf("docker stop %s: %w", c.Name, err)
	}
	// Clean up stale ControlMaster socket (if any). The SSH connection is
	// dead now that the container is stopped.
	if err := cleanupControlSocket(ctx, c.Client, c.Name); err != nil {
		return err
	}
	c.State = "exited"
	return nil
}

// Purge stops and removes the container, cleaning up SSH config and git remotes.
func (c *Container) Purge(ctx context.Context, stdout, stderr io.Writer) error {
	_, containerErr := c.Runtime.Run(ctx, "", "inspect", c.Name)
	containerExists := containerErr == nil
	var anyRemoteExists bool
	for i := range c.Repos {
		if _, err := c.runCmd(ctx, c.Repos[i].GitRoot, []string{"git", "remote", "get-url", c.Name}); err == nil {
			anyRemoteExists = true
			break
		}
	}
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	sshConf := filepath.Join(sshConfigDir, c.Name+".conf")
	sshKnown := filepath.Join(sshConfigDir, c.Name+".known_hosts")
	_, sshConfErr := os.Stat(sshConf)
	_, sshKnownErr := os.Stat(sshKnown)
	sshExists := sshConfErr == nil || sshKnownErr == nil

	if !containerExists && !anyRemoteExists && !sshExists {
		return fmt.Errorf("%s not found", c.Name)
	}

	var retErr error
	// Clean up non-ephemeral Tailscale node.
	if containerExists {
		if !c.Tailscale {
			tsLabel, _ := c.Runtime.Run(ctx, "", "inspect", "--format", `{{index .Config.Labels "md.tailscale"}}`, c.Name)
			c.Tailscale = tsLabel == "1"
		}
		if c.Tailscale {
			ephLabel, _ := c.Runtime.Run(ctx, "", "inspect", "--format", `{{index .Config.Labels "md.tailscale_ephemeral"}}`, c.Name)
			if ephLabel != "1" {
				deviceID, err := c.tailscaleDeviceID(ctx)
				switch {
				case err != nil:
					retErr = errors.Join(retErr, fmt.Errorf("reading Tailscale device ID: %w", err))
				case deviceID != "":
					_, _ = fmt.Fprintln(stdout, "- Removing Tailscale node from tailnet...")
					if err := deleteTailscaleDevice(ctx, c.TailscaleAPIKey, deviceID); err != nil {
						retErr = errors.Join(retErr, fmt.Errorf("removing Tailscale node %s: %w", deviceID, err))
					}
				}
			}
		}
	}
	if retErr != nil {
		return retErr
	}

	if err2 := os.Remove(sshConf); err2 != nil && !os.IsNotExist(err2) {
		retErr = err2
	}
	if err2 := os.Remove(sshKnown); err2 != nil && !os.IsNotExist(err2) {
		retErr = errors.Join(retErr, err2)
	}

	for i := range c.Repos {
		gitRoot := c.Repos[i].GitRoot
		if _, err := c.runCmd(ctx, gitRoot, []string{"git", "remote", "get-url", c.Name}); err == nil {
			if _, err := c.runCmd(ctx, gitRoot, []string{"git", "remote", "remove", c.Name}); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}
	if containerExists {
		if _, err := c.Runtime.Run(ctx, "", "rm", "-f", "-v", c.Name); err != nil {
			retErr = err
		}
	}
	_, _ = fmt.Fprintf(stdout, "Removed %s\n", c.Name)
	return retErr
}

// Push force-pushes local state for Repos[repoIdx] into the container,
// saving a backup of the container state and returning the backup branch name.
// All branches in Branches are synced.
func (c *Container) Push(ctx context.Context, stdout, stderr io.Writer, repoIdx int) (string, error) {
	if len(c.Repos) == 0 {
		return "", errors.New("container has no repos")
	}
	if repoIdx < 0 || repoIdx >= len(c.Repos) {
		return "", fmt.Errorf("repo index %d out of range [0, %d)", repoIdx, len(c.Repos))
	}
	if err := c.checkContainerState(ctx, containerMustBeRunning); err != nil {
		return "", err
	}
	r := &c.Repos[repoIdx]
	if err := r.validateMappedBranchUpstreams(ctx, c.Logger); err != nil {
		return "", err
	}
	g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
	mp := shellQuote(r.ContainerPath)
	identity, err := r.gitIdentity(ctx, c.Logger)
	if err != nil {
		return "", err
	}
	backupBranch := "backup-" + time.Now().Format("20060102-150405")
	// Do the dirty check, optional commit, and backup branch creation in one
	// remote shell so every step observes the same index and HEAD.
	backupCommands := make([]string, 0, 7+2*len(r.Branches))
	backupCommands = append(backupCommands, "cd "+mp)
	backupCommands = append(backupCommands, gitIdentityCommands("--local", identity)...)
	backupCommands = append(backupCommands,
		"git add .",
		"diff_status=0",
		"{ git diff --quiet HEAD -- . || diff_status=$?; if [ \"$diff_status\" -gt 1 ]; then exit \"$diff_status\"; fi; if [ \"$diff_status\" -ne 0 ]; then git commit -q -m 'Backup before push'; fi; }",
	)
	backupCommands = append(backupCommands, backupContainerBranchesCommands(r.Branches, backupBranch)...)
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, strings.Join(backupCommands, " && ")), stdout, stderr); err != nil {
		return "", fmt.Errorf("backing up container state: %w", err)
	}
	// Refuse if there are pending local changes on any mapped branch.
	currentBranch, err := g.RunGit(ctx, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("reading current branch: %w", err)
	}
	if slices.Contains(r.Branches, currentBranch) {
		status, err := g.RunGit(ctx, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return "", fmt.Errorf("checking local changes: %w", err)
		}
		if status != "" {
			return "", fmt.Errorf("there are pending changes on branch %s locally. Please commit or stash them before pushing", currentBranch)
		}
	}
	bases, err := c.pushMappedBranchRefs(ctx, stdout, stderr, r)
	if err != nil {
		return "", err
	}
	// Configure remotes before moving the branches; the branch resets depend on the
	// refs and upstreams set by the preceding push.
	if err := c.configureContainerRemotes(ctx, stdout, stderr, repoIdx, true, containerBranchSetupCommands(bases)...); err != nil {
		return "", err
	}
	if err := c.recordHostBranchSyncPoints(ctx, r); err != nil {
		return "", err
	}
	// Update host's remote-tracking refs for all branches.
	for _, b := range r.Branches {
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "update-ref", remoteTrackingRef(c.Name, b), "refs/heads/" + b}, stdout, stderr); err != nil {
			c.Logger.WarnContext(ctx, "failed to update host tracking ref", "branch", b, "err", err)
		}
	}
	return backupBranch, nil
}

// Fetch fetches the mapped branches of Repos[repoIdx] from the container and
// updates the host's remote-tracking refs without integrating them. It returns
// the exact branch tips fetched and does not change the base reported by
// [Container.Diff].
//
// The zero FetchOpts fetches only what the container already committed, which
// leaves its history and working tree untouched. Set FetchOpts.Commit to commit
// the container's pending changes first, so the host receives them too.
//
// opts may be nil, which selects the default of the zero FetchOpts.
func (c *Container) Fetch(ctx context.Context, stdout, stderr io.Writer, repoIdx int, opts *FetchOpts) ([]FetchedBranch, error) {
	if opts == nil {
		opts = &FetchOpts{}
	}
	if len(c.Repos) == 0 {
		return nil, errors.New("container has no repos")
	}
	if repoIdx < 0 || repoIdx >= len(c.Repos) {
		return nil, fmt.Errorf("repo index %d out of range [0, %d)", repoIdx, len(c.Repos))
	}
	if err := c.checkContainerState(ctx, containerMustBeRunning); err != nil {
		return nil, err
	}
	r := &c.Repos[repoIdx]
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return nil, err
	}
	commitCommands, err := c.containerGitConfigCommands(ctx, repoIdx, true)
	if err != nil {
		return nil, err
	}
	commitPrefix := strings.Join(commitCommands, " && ")
	commitMsg := "Pull from md"
	verifyFlag := ""
	if opts.NoVerify {
		verifyFlag = " --no-verify"
	}
	switch {
	case !opts.Commit:
		// The container's remote and upstream configuration still has to match
		// the host, since the fetched refs and their upstreams depend on it.
		if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, commitPrefix), stdout, stderr); err != nil {
			return nil, fmt.Errorf("configuring remotes for %s: %w", r.ContainerPath, err)
		}
	case opts.Provider == nil:
		// With the fixed commit message, one remote script can stage, check, and
		// commit. A clean tree exits successfully; real git errors still propagate.
		commitCmd := commitPrefix + " && git add . && diff_status=0 && { git diff --quiet HEAD -- . || diff_status=$?; if [ \"$diff_status\" -eq 0 ]; then exit 0; fi; if [ \"$diff_status\" -gt 1 ]; then exit \"$diff_status\"; fi; echo " + shellQuote(commitMsg) + " | git commit -a -q" + verifyFlag + " -F -; }"
		if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, commitCmd), stdout, stderr); err != nil {
			return nil, fmt.Errorf("committing in container: %w", err)
		}
	default:
		if _, err := c.runCmd(ctx, "", c.SSHCommand(nil, commitPrefix+" && git add . && git diff --quiet HEAD -- .")); err != nil {
			metadata := c.gatherGitMetadata(ctx, r)
			diff := c.gatherGitDiff(ctx, r)
			msgOpts := &git.CommitMsgOptions{ContextTokens: opts.ContextTokens, Progress: stderr}
			if msg, err := git.GenerateCommitMsg(ctx, opts.Provider, metadata, diff, msgOpts); err != nil {
				c.Logger.Log(ctx, slog.LevelWarn, "failed to generate commit message", "err", err)
			} else if msg != "" {
				commitMsg = msg
			}
			commitCmd := commitPrefix + " && echo " + shellQuote(commitMsg) + " | git commit -a -q" + verifyFlag + " -F -"
			if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, commitCmd), stdout, stderr); err != nil {
				return nil, fmt.Errorf("committing in container: %w", err)
			}
		}
	}
	// Fetch all mapped branches over one connection. Explicit forced refspecs
	// preserve rewritten container history and avoid ambiguous short ref names.
	if len(r.Branches) == 0 {
		return nil, nil
	}
	fetchArgs := []string{"git", "fetch", "-q", c.Name}
	for _, b := range r.Branches {
		fetchArgs = append(fetchArgs, "+refs/heads/"+b+":"+remoteTrackingRef(c.Name, b))
	}
	if err := c.runCmdOut(ctx, r.GitRoot, fetchArgs, stdout, stderr); err != nil {
		return nil, fmt.Errorf("fetching mapped branches %s: %w", strings.Join(r.Branches, ", "), err)
	}
	g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
	fetched := make([]FetchedBranch, 0, len(r.Branches))
	for _, b := range r.Branches {
		commit, err := g.RevParse(ctx, remoteTrackingRef(c.Name, b))
		if err != nil {
			return nil, fmt.Errorf("reading fetched branch %s: %w", b, err)
		}
		fetched = append(fetched, FetchedBranch{BranchName: b, CommitHash: commit})
	}
	return fetched, nil
}

// Pull fetches changes from the container and integrates all mapped branches
// into the local repository.
//
// opts controls AI commit message generation. Pass nil to use a default message.
func (c *Container) Pull(ctx context.Context, stdout, stderr io.Writer, repoIdx int, opts *PullOpts) error {
	if opts == nil {
		opts = &PullOpts{}
	}
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	if repoIdx < 0 || repoIdx >= len(c.Repos) {
		return fmt.Errorf("repo index %d out of range [0, %d)", repoIdx, len(c.Repos))
	}
	r := &c.Repos[repoIdx]
	g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
	rebaseActive, err := gitRebaseInProgress(ctx, g)
	if err != nil {
		return fmt.Errorf("checking for an existing rebase: %w", err)
	}
	if rebaseActive {
		return errors.New("a rebase is already in progress locally. Complete or abort it before pulling")
	}
	currentBranch, err := g.RunGit(ctx, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("reading current branch: %w", err)
	}
	status, err := g.RunGit(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("checking local changes: %w", err)
	}
	if status != "" {
		if currentBranch != "" {
			return fmt.Errorf("there are pending changes on branch %s locally. Please commit or stash them before pulling", currentBranch)
		}
		return errors.New("there are pending changes locally. Please commit or stash them before pulling")
	}
	previousTips := make(map[string]string, len(r.Branches))
	for _, branch := range r.Branches {
		ref := remoteTrackingRef(c.Name, branch)
		exists, err := g.RefExists(ctx, ref)
		if err != nil {
			return fmt.Errorf("checking previous container ref %s: %w", ref, err)
		}
		if !exists {
			continue
		}
		tip, err := g.RunGit(ctx, "rev-parse", ref)
		if err != nil {
			return fmt.Errorf("reading previous container ref %s: %w", ref, err)
		}
		previousTips[branch] = tip
	}
	// Pull integrates the container's work into the host branch, so anything
	// still pending in the container has to become a commit first.
	fetched, err := c.Fetch(ctx, stdout, stderr, repoIdx, &FetchOpts{Provider: opts.Provider, ContextTokens: opts.ContextTokens, Commit: true, NoVerify: opts.NoVerify})
	if err != nil {
		return err
	}
	// Save the original commit when detached so it can be restored after integration.
	origCommit := ""
	if currentBranch == "" {
		origCommit, err = g.RevParse(ctx, "HEAD")
		if err != nil {
			return fmt.Errorf("reading detached HEAD: %w", err)
		}
	}
	integrationErr := c.pullBranches(ctx, stdout, stderr, g, r.Branches, previousTips)
	if integrationErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		recoveryErr := c.recoverPullCheckout(cleanupCtx, stdout, stderr, g, currentBranch, origCommit)
		return errors.Join(integrationErr, recoveryErr)
	}
	restoreErr := c.restorePullCheckout(context.WithoutCancel(ctx), stdout, stderr, g, currentBranch, origCommit)
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	syncErr := c.recordFetchedBranchSyncPoints(syncCtx, r, fetched)
	return errors.Join(syncErr, restoreErr)
}

// Diff writes the checked-out container branch's changes, including untracked
// files, after verifying every mapped branch still matches the host.
//
// By default it reports the changes not yet integrated into the host branch,
// using the point established by container creation or the last successful
// Push, Pull or Fork. Set DiffOpts.Full to report the whole branch instead.
//
// opts may be nil, which selects the default of the zero DiffOpts.
//
// When stdout is a terminal, a TTY is allocated so git's pager and colors work.
func (c *Container) Diff(ctx context.Context, stdout, stderr io.Writer, repoIdx int, opts *DiffOpts) error {
	if opts == nil {
		opts = &DiffOpts{}
	}
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	if repoIdx < 0 || repoIdx >= len(c.Repos) {
		return fmt.Errorf("repo index %d out of range [0, %d)", repoIdx, len(c.Repos))
	}
	if err := c.checkContainerState(ctx, containerMustBeRunning); err != nil {
		return err
	}
	repo := &c.Repos[repoIdx]
	if repo.DefaultRemote == "" || repo.DefaultBranch == "" {
		if err := repo.resolveDefaults(ctx, c.Logger); err != nil {
			return fmt.Errorf("resolving repository defaults for %q: %w", repo.GitRoot, err)
		}
	}
	if err := c.validateContainerBranchUpstreams(ctx, repoIdx); err != nil {
		return err
	}
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return err
	}
	sshOpts := []string{"-q"}
	isTTY := false
	if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		sshOpts = append(sshOpts, "-t")
		isTTY = true
	}
	req := &diffRequest{
		repo:          repo.ContainerPath,
		primaryBranch: repo.Branches[0],
		defaultRemote: repo.DefaultRemote,
		defaultBranch: repo.DefaultBranch,
		exitOnDiff:    slices.Contains(opts.Args, "--exit-code") || slices.Contains(opts.Args, "--quiet"),
		extraArgs:     opts.Args,
		full:          opts.Full,
		quiet:         slices.Contains(opts.Args, "--quiet"),
	}
	sshArgs := c.SSHCommand(sshOpts, gitDiffCommand(req))
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "cmd", sshArgs)
	cmd := exec.CommandContext(ctx, sshArgs[0]) //nolint:gosec // args are from trusted SSH config
	cmd.Env = c.commandEnv()
	if isTTY {
		cmd.Stdin = os.Stdin
	}
	sshPath, err := exec.LookPath(sshArgs[0])
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}
	cmd.Path = sshPath
	cmd.Args = sshArgs
	cmd.Stdout = stdout
	var sshStderr bytes.Buffer
	cmd.Stderr = &sshStderr
	runErr := cmd.Run()
	differencesFound := false
	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok && exitErr.ExitCode() == diffFoundSSHExitCode {
			differencesFound = true
		} else {
			return commandErrorWithStderr(fmt.Sprintf("running diff in container %q over SSH", c.Name), runErr, sshStderr.String())
		}
	}
	if stderr != nil {
		if _, err := stderr.Write(sshStderr.Bytes()); err != nil {
			return fmt.Errorf("writing SSH stderr: %w", err)
		}
	}
	if differencesFound {
		return ErrDiffFound
	}
	return nil
}

// diffRequest is the resolved input of one container diff.
type diffRequest struct {
	repo          string
	primaryBranch string
	defaultRemote string
	defaultBranch string
	extraArgs     []string
	exitOnDiff    bool
	full          bool
	quiet         bool
}

func gitDiffCommand(req *diffRequest) string {
	diffArgs := ""
	if len(req.extraArgs) != 0 {
		quotedArgs := make([]string, len(req.extraArgs))
		for i, a := range req.extraArgs {
			quotedArgs[i] = shellQuote(a)
		}
		diffArgs = " " + strings.Join(quotedArgs, " ")
	}
	exitOnDiffFlag := "0"
	if req.exitOnDiff {
		exitOnDiffFlag = "1"
	}
	baseRefCommand := gitIntegrationBaseRefCommand(req.primaryBranch, req.defaultRemote, req.defaultBranch, req.quiet)
	if req.full {
		baseRefCommand = gitDiffBaseRefCommand(req.primaryBranch, req.defaultRemote, req.defaultBranch)
	}
	commands := []string{
		"cd " + shellQuote(req.repo) + " || exit 2",
		baseRefCommand,
		"export GIT_OPTIONAL_LOCKS=0",
		`index_path=$(git rev-parse --git-path index) || exit 2`,
		`tmp_index=$(mktemp) || exit 2`,
		`diff_stdout=$(mktemp) || exit 2`,
		`diff_stderr=$(mktemp) || exit 2`,
		`untracked_paths=$(mktemp) || exit 2`,
		`trap 'rm -f "$tmp_index" "$diff_stdout" "$diff_stderr" "$untracked_paths"' EXIT`,
		// Preserve the index timestamp so Git keeps its racy-clean checks valid.
		// Recreating it before a retry also drops intent-to-add entries for files
		// that vanished during the previous diff.
		`prepare_diff_index() {
	cp -p "$index_path" "$tmp_index" &&
		git ls-files -z --others --exclude-standard -- . > "$untracked_paths" || return 1
	while IFS= read -r -d '' path; do
		GIT_INDEX_FILE="$tmp_index" git add -N -- "$path" || return 1
	done < "$untracked_paths"
}`,
		`prepare_diff_index || exit 2`,
		"diff_attempt=0",
		`while :; do
	: > "$diff_stdout"
	: > "$diff_stderr"
	diff_status=0
	if [ -t 1 ]; then
		exec 3>&1
	else
		exec 3> "$diff_stdout"
	fi
	GIT_INDEX_FILE="$tmp_index" git diff "$diff_base_ref"` + diffArgs + ` -- . >&3 2> "$diff_stderr" || diff_status=$?
	exec 3>&-
	if [ "$diff_status" -le 1 ]; then
		if [ ! -t 1 ]; then cat "$diff_stdout" || exit 2; fi
		cat "$diff_stderr" >&2 || exit 2
		break
	fi
	vanished_untracked=0
	while IFS= read -r -d '' path; do
		if [ ! -e "$path" ] && [ ! -L "$path" ]; then
			vanished_untracked=1
			break
		fi
	done < "$untracked_paths"
	if [ "$diff_attempt" -ge 2 ] || [ "$vanished_untracked" -eq 0 ] || ! grep -q "^fatal: stat .*: No such file or directory$" "$diff_stderr"; then
		cat "$diff_stderr" >&2
		exit 2
	fi
	diff_attempt=$((diff_attempt + 1))
	prepare_diff_index || exit 2
done`,
		"if [ " + exitOnDiffFlag + ` -eq 1 ] && [ "$diff_status" -eq 1 ]; then exit ` + strconv.Itoa(diffFoundSSHExitCode) + `; fi`,
	}
	return strings.Join(commands, "; ")
}

// forkRepoPlan is the validated result of matching a fork spec against the
// source container's repos: destination primaries for the source repos (in
// source order) and the resolved new repos to add (in caller order).
type forkRepoPlan struct {
	srcDest    []string
	extraRepos []Repo
	extraDest  []string
}

type forkPreflight struct {
	sourceUpstreams   []branchUpstreamSpec
	sourceSyncRefspec [][]string
	extraBases        [][]containerBranchBase
	extraRefspecs     [][]string
}

func preflightForkRepos(ctx context.Context, logger *slog.Logger, sourceRepos, extraRepos, forkRepos []Repo) (*forkPreflight, error) {
	preflight := &forkPreflight{
		sourceUpstreams:   make([]branchUpstreamSpec, len(sourceRepos)),
		sourceSyncRefspec: make([][]string, len(sourceRepos)),
		extraBases:        make([][]containerBranchBase, len(extraRepos)),
		extraRefspecs:     make([][]string, len(extraRepos)),
	}
	for i := range sourceRepos {
		r := &sourceRepos[i]
		if err := r.validateMappedBranchUpstreams(ctx, logger); err != nil {
			return nil, fmt.Errorf("preflight source repo %s: %w", r.GitRoot, err)
		}
		upstream, err := r.mappedBranchUpstreamSpec(ctx, logger, r.Branches[0])
		if err != nil {
			return nil, fmt.Errorf("preflight source repo %s: %w", r.GitRoot, err)
		}
		preflight.sourceUpstreams[i] = upstream
		refspecs, err := r.containerSyncRefspecs(ctx, logger)
		if err != nil {
			return nil, fmt.Errorf("preflight source repo %s refs: %w", r.GitRoot, err)
		}
		preflight.sourceSyncRefspec[i] = refspecs
	}
	for i := range extraRepos {
		src := &extraRepos[i]
		dst := &forkRepos[len(sourceRepos)+i]
		bases := make([]containerBranchBase, len(src.Branches))
		refspecs := make([]string, len(src.Branches))
		for j, branch := range src.Branches {
			base, err := src.resolveForkExtraBranchBase(ctx, logger, branch, dst.Branches[j])
			if err != nil {
				return nil, fmt.Errorf("preflight extra repo %s branch %q: %w", src.GitRoot, branch, err)
			}
			bases[j] = base
			refspecs[j] = "refs/heads/" + branch + ":" + base.destination
		}
		syncRefspecs, err := src.containerSyncRefspecs(ctx, logger)
		if err != nil {
			return nil, fmt.Errorf("preflight extra repo %s refs: %w", src.GitRoot, err)
		}
		preflight.extraBases[i] = bases
		preflight.extraRefspecs[i] = appendUniqueRefspecs(refspecs, syncRefspecs...)
	}
	return preflight, nil
}

// planFork validates spec against sourceRepos and resolves the new repos. Every
// source repo must have a spec entry whose SourceBranches (when given) match the
// recorded branches; entries for repos not in sourceRepos become new repos, with
// their source branches defaulting to the repo's default upstream branch.
func planFork(ctx context.Context, logger *slog.Logger, excludedRemote string, sourceRepos []Repo, spec []ForkRepo) (*forkRepoPlan, error) {
	// Index the spec by GitRoot, rejecting duplicates and empty destinations.
	byRoot := make(map[string]ForkRepo, len(spec))
	for _, fr := range spec {
		if fr.DestPrimary == "" {
			return nil, fmt.Errorf("repo %s: empty DestPrimary in ForkOpts.Repos", fr.GitRoot)
		}
		if _, dup := byRoot[fr.GitRoot]; dup {
			return nil, fmt.Errorf("repo %s listed twice in ForkOpts.Repos", fr.GitRoot)
		}
		byRoot[fr.GitRoot] = fr
	}

	// Every source-container repo must have a spec, and its carried branches must
	// match what the snapshot recorded.
	sourceRoots := make(map[string]struct{}, len(sourceRepos))
	srcDest := make([]string, len(sourceRepos))
	for i := range sourceRepos {
		r := &sourceRepos[i]
		sourceRoots[r.GitRoot] = struct{}{}
		fr, ok := byRoot[r.GitRoot]
		if !ok {
			return nil, fmt.Errorf("source-container repo %s missing from ForkOpts.Repos", r.GitRoot)
		}
		if len(fr.SourceBranches) != 0 && !slices.Equal(fr.SourceBranches, r.Branches) {
			return nil, fmt.Errorf("repo %s: SourceBranches %v do not match the source container's %v", r.GitRoot, fr.SourceBranches, r.Branches)
		}
		// The fork gets its own branch, so the destination must be a new name.
		// Reusing any mapped branch would map one host branch into two
		// containers, and Fork force-resets the destination host branch before
		// renaming the container's primary onto it.
		if slices.Contains(r.Branches, fr.DestPrimary) {
			return nil, fmt.Errorf("repo %s: DestPrimary %q is already a mapped branch of the source container; choose a new branch name", r.GitRoot, fr.DestPrimary)
		}
		srcDest[i] = fr.DestPrimary
	}

	// Remaining specs are new repos, kept in caller order. Default the source
	// branches to the repo's default upstream branch (the branch to push from the
	// host), matching how Launch resolves defaults for its repos.
	plan := &forkRepoPlan{srcDest: srcDest}
	for _, fr := range spec {
		if _, isSource := sourceRoots[fr.GitRoot]; isSource {
			continue
		}
		r := Repo{
			GitRoot:       fr.GitRoot,
			Branches:      slices.Clone(fr.SourceBranches),
			ContainerPath: fr.ContainerPath,
			TagRegexp:     fr.TagRegexp,
		}
		r.populateContainerPath()
		r.ContainerPath = ResolveContainerPath(r.ContainerPath)
		if err := r.resolveRemotes(ctx, logger, excludedRemote); err != nil {
			return nil, fmt.Errorf("resolving remotes for extra repo %s: %w", r.GitRoot, err)
		}
		if err := r.resolveDefaults(ctx, logger); err != nil {
			return nil, fmt.Errorf("resolving defaults for extra repo %s: %w", r.GitRoot, err)
		}
		if len(r.Branches) == 0 {
			r.Branches = []string{r.DefaultBranch}
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("extra repo %s: %w", r.GitRoot, err)
		}
		if slices.Contains(r.Branches, fr.DestPrimary) {
			return nil, fmt.Errorf("extra repo %s: DestPrimary %q is already one of its source branches; choose a new branch name", r.GitRoot, fr.DestPrimary)
		}
		plan.extraRepos = append(plan.extraRepos, r)
		plan.extraDest = append(plan.extraDest, fr.DestPrimary)
	}
	return plan, nil
}

// Fork snapshots a running or stopped container and creates a new one where each mapped
// repository is checked out on a new primary branch.
//
// The snapshot preserves the container's entire filesystem (installed
// packages, build artifacts, etc.) while giving each repo a fresh primary
// branch that diverges from the source container's working state. Extra mapped
// branches keep their original names.
//
// Branch naming: the caller supplies the full repo set via ForkOpts.Repos, each
// entry naming its DestPrimary. Fork uses those names verbatim and only rejects
// a name the repo already maps, which it would destroy. Allocating a free name
// and avoiding other containers' branches belongs to the caller, symmetric with
// Launch, where the caller owns branch names too.
//
// If setup fails, Fork removes the partial container and restores or deletes
// destination host branches that still point to the commits written by this
// attempt. Branches moved concurrently are preserved and reported.
func (c *Container) Fork(ctx context.Context, stdout, stderr io.Writer, opts *ForkOpts) (_ *Container, retErr error) {
	attempt := forkAttempt{source: c}
	defer func() {
		if retErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, attempt.rollback(cleanupCtx))
	}()

	if err := c.checkContainerState(ctx, containerMayBeStopped); err != nil {
		return nil, err
	}

	plan, err := planFork(ctx, c.Logger, c.Name, c.Repos, opts.Repos)
	if err != nil {
		return nil, err
	}
	extraRepos := plan.extraRepos

	// Build the destination repo list: source repos (c.Repos order) then extras,
	// each with its primary renamed to the caller's DestPrimary. Non-primary
	// branches keep their source names.
	allSrc := append(slices.Clone(c.Repos), extraRepos...)
	dests := append(slices.Clone(plan.srcDest), plan.extraDest...)
	forkRepos := slices.Clone(allSrc)
	for i := range allSrc {
		branches := slices.Clone(allSrc[i].Branches)
		branches[0] = dests[i]
		forkRepos[i].Branches = branches
	}
	preflight, err := preflightForkRepos(ctx, c.Logger, c.Repos, extraRepos, forkRepos)
	if err != nil {
		return nil, err
	}

	// Snapshot the source container, stripping all labels so
	// launchContainer sets them fresh on the forked container.
	// docker commit bakes container labels into the image; any label
	// not explicitly re-set by launchContainer would leak through.
	snapshotImage := "md-fork-" + c.Name
	attempt.snapshotImage = snapshotImage
	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Snapshotting container %s → %s ...\n", c.Name, snapshotImage)
	}
	// Inspect the source container to discover all label keys.
	labelCSV, err := c.Runtime.Run(ctx, "", "inspect", "--format", `{{range $k, $v := .Config.Labels}}{{$k}} {{end}}`, c.Name)
	if err != nil {
		return nil, fmt.Errorf("inspecting labels: %w", err)
	}
	commitArgs := []string{"commit"}
	for _, change := range forkSnapshotConfigChanges(labelCSV) {
		commitArgs = append(commitArgs, "--change", change)
	}
	commitArgs = append(commitArgs, c.Name, snapshotImage)
	if _, err := c.Runtime.Run(ctx, "", commitArgs...); err != nil {
		return nil, fmt.Errorf("docker commit: %w", err)
	}
	attempt.snapshotCreated = true

	// Create the new container handle with destination branches.
	fork, err := c.Container(forkRepos...)
	if err != nil {
		return nil, fmt.Errorf("fork container: %w", err)
	}
	attempt.fork = fork

	// Start the new container from the snapshot image.
	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Starting forked container %s ...\n", fork.Name)
	}
	startOpts := opts.startOptions()
	fork.prepareTailscaleAuthKey(ctx, stdout, startOpts)
	fork.Display = startOpts.Display
	fork.Tailscale = startOpts.Tailscale
	fork.USB = startOpts.USB
	fork.Sudo = startOpts.Sudo
	if err := fork.launchContainer(ctx, stdout, stderr, startOpts, snapshotImage); err != nil {
		return nil, err
	}
	if !startOpts.Sudo {
		if err := fork.revokeSudo(ctx); err != nil {
			return nil, err
		}
	}
	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Removing temporary snapshot tag %s ...\n", snapshotImage)
	}
	if err := fork.untagImage(ctx, snapshotImage); err != nil {
		return nil, err
	}
	attempt.snapshotCreated = false

	if startOpts.Tailscale && startOpts.TailscaleAuthKey == "" {
		if _, err := fork.tryReadTailscaleAuthURL(ctx, stdout); err != nil {
			return nil, fmt.Errorf("reading Tailscale auth URL: %w", err)
		}
	}

	// Wait for SSH and set up repos.
	addr := fmt.Sprintf("localhost:%d", fork.SSHPort)
	if err := waitForTCP(ctx, addr, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return nil, fmt.Errorf("waiting for SSH on forked container: %w", err)
	}
	if err := fork.waitForSSH(ctx, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return nil, fmt.Errorf("SSH handshake on forked container: %w", err)
	}
	if err := fork.configureGlobalGitIdentity(ctx, stdout, stderr); err != nil {
		return nil, err
	}

	// Restore ownership before running Git. Under rootless podman, `commit` does
	// not round-trip the keep-id uid mapping, so every file the source
	// container owned as `user` re-appears root-owned in the fork. For the
	// repos that breaks git with "detected dubious ownership" and leaves them
	// unwritable during git-receive-pack; elsewhere in the home it would break
	// a forked agent's writes. A fresh home has no legitimately root-owned
	// files, so restoring every root-owned file to `user` (as root, which
	// start.sh runs as) is both safe and complete. See docs/ROOTLESS.md.
	//
	// `find -xdev` stays on the container's own filesystem, so it never
	// descends into a bind-mounted host directory (which keep-id presents as
	// user-owned anyway) — host ownership is never rewritten, the reason `:U`
	// was rejected. `-uid 0` restores only the collapsed files.
	if c.Runtime.IsRootless() {
		if _, err := c.Runtime.Run(ctx, "", "exec", "--user", "0:0", fork.Name, "find", containerHomeDir, "-xdev", "-uid", "0", "-exec", "chown", "user:user", "{}", "+"); err != nil {
			return nil, fmt.Errorf("restoring ownership on forked container: %w", err)
		}
	}

	// Refresh cached host refs directly into the new fork. This also supports a
	// stopped source container, which cannot be updated before snapshotting.
	for i := range c.Repos {
		if err := fork.pushContainerRefs(ctx, &c.Repos[i], preflight.sourceSyncRefspec[i]); err != nil {
			return nil, fmt.Errorf("syncing fork remote branches: %w", err)
		}
	}

	// The snapshot preserves source refs, so fetch them from the running fork
	// instead of restarting the stopped source container.
	if !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- Creating local branches ...")
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		if err := c.runCmdOut(ctx, r.GitRoot, append([]string{"git", "fetch", "-q", fork.Name}, r.Branches...), stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from forked container: %w", r.ContainerPath, err)
		}
		fetchedRef := fork.Name + "/" + r.Branches[0]
		newBranch := fork.Repos[i].Branches[0]
		change, err := r.createForkHostBranch(ctx, c.Logger, newBranch, fetchedRef, preflight.sourceUpstreams[i])
		attempt.hostBranches = append(attempt.hostBranches, change)
		if err != nil {
			return nil, fmt.Errorf("creating host branch for %s: %w", r.ContainerPath, err)
		}
	}

	sshCommandDeadline := time.Now().Add(containerSSHCommandRetryTimeout)

	// Send .env into the forked container.
	var envContent []byte
	for i := range forkRepos {
		data, err := os.ReadFile(filepath.Join(forkRepos[i].GitRoot, ".env"))
		if err != nil {
			continue
		}
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		envContent = append(envContent, data...)
	}
	if len(startOpts.ExtraEnv) > 0 {
		extraEnv, err := renderExtraEnv(startOpts.ExtraEnv)
		if err != nil {
			return nil, err
		}
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		envContent = append(envContent, extraEnv...)
	}
	sshEnvArgs := fork.SSHCommand(nil, "cat > /home/user/.env")
	fork.Logger.Log(ctx, slog.LevelDebug, "ssh", "cmd", sshEnvArgs)
	for {
		cmd := exec.CommandContext(ctx, sshEnvArgs[0], sshEnvArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Env = fork.commandEnv()
		cmd.Stdin = bytes.NewReader(envContent)
		out, err := cmd.CombinedOutput()
		if err == nil {
			break
		}
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok || exitErr.ExitCode() != 255 || time.Now().After(sshCommandDeadline) {
			return nil, fmt.Errorf("copying .env to forked container: %w\n%s", err, out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Inside the forked container: rename source repo branches and push extra
	// repos as new.
	if !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- Setting up branches in forked container ...")
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		oldPrimary := r.Branches[0]
		newPrimary := fork.Repos[i].Branches[0]
		// Apply the host's current remote and upstream configuration after
		// renaming the snapshotted source branch. Record every committed branch
		// tip as freshly synchronized because the host fetched them above.
		renameCommands := make([]string, 1, len(fork.Repos[i].Branches)+2)
		renameCommands[0] = forkPrimaryBranchSetupCommand(oldPrimary, newPrimary)
		renameCommands = append(renameCommands, forkSyncPointCommands(oldPrimary, fork.Repos[i].Branches)...)
		if err := fork.configureContainerRemotes(ctx, stdout, stderr, i, true, renameCommands...); err != nil {
			return nil, fmt.Errorf("renaming branch for %s: %w", r.ContainerPath, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{"git", "fetch", "-q", fork.Name, newPrimary}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from fork: %w", newPrimary, err)
		}
	}
	// Push extra repos into the container using source branches, set up
	// destination branches inside.
	nSrc := len(c.Repos)
	for i := range extraRepos {
		src := &extraRepos[i]
		dst := &forkRepos[nSrc+i]
		mp := shellQuote(src.ContainerPath)

		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, "git init -q "+mp), stdout, stderr); err != nil {
			return nil, fmt.Errorf("init extra repo %s in container: %w", src.ContainerPath, err)
		}
		bases := preflight.extraBases[i]
		refspecs := appendUniqueRefspecs(preflight.extraRefspecs[i], branchSyncRefspecs(src.Branches, dst.Branches)...)
		if err := c.pushRefspecs(ctx, src.GitRoot, fork.Name, refspecs, true, stdout, stderr); err != nil {
			return nil, fmt.Errorf("push extra repo %s: %w", src.ContainerPath, err)
		}
		primaryBase := bases[0]
		upstream := branchUpstreamSpec{
			remote:     primaryBase.upstreamRemote,
			branch:     primaryBase.upstreamBranch,
			pushRemote: primaryBase.pushRemote,
		}
		change, err := src.createForkHostBranch(ctx, c.Logger, dst.Branches[0], "refs/heads/"+src.Branches[0], upstream)
		attempt.hostBranches = append(attempt.hostBranches, change)
		if err != nil {
			return nil, fmt.Errorf("creating host branch for extra repo %s: %w", src.ContainerPath, err)
		}
		if err := fork.configureContainerRemotes(ctx, stdout, stderr, nSrc+i, false, containerBranchSetupCommands(bases)...); err != nil {
			return nil, fmt.Errorf("setting up extra repo %s: %w", src.ContainerPath, err)
		}
		if err := c.runCmdOut(ctx, dst.GitRoot, []string{"git", "fetch", "-q", fork.Name, dst.Branches[0]}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from extra repo fork: %w", dst.Branches[0], err)
		}
	}

	fork.State = "running"
	return fork, nil
}

// forkPrimaryBranchSetupCommand returns a shell command that renames the forked
// primary branch, including when the source branch is mid-rebase.
func forkPrimaryBranchSetupCommand(oldBranch, newBranch string) string {
	oldRef := "refs/heads/" + oldBranch
	newRef := "refs/heads/" + newBranch
	oldConfig := "branch." + oldBranch
	newConfig := "branch." + newBranch
	return strings.Join([]string{
		"old_ref=" + shellQuote(oldRef),
		"new_ref=" + shellQuote(newRef),
		"old_config=" + shellQuote(oldConfig),
		"new_config=" + shellQuote(newConfig),
		"rebase_head=",
		`for candidate in "$(git rev-parse --git-path rebase-merge/head-name)" "$(git rev-parse --git-path rebase-apply/head-name)"; do if [ -s "$candidate" ] && [ "$(cat "$candidate")" = "$old_ref" ]; then rebase_head=$candidate; break; fi; done`,
		`if [ -n "$rebase_head" ]; then git update-ref "$new_ref" "$old_ref" "" && printf '%s\n' "$new_ref" > "$rebase_head" && if git config --get "$old_config.remote" >/dev/null || git config --get "$old_config.merge" >/dev/null; then git config --rename-section "$old_config" "$new_config"; fi && git branch -q -D ` + shellQuote(oldBranch) + `; else git branch -m ` + shellQuote(oldBranch) + ` ` + shellQuote(newBranch) + `; fi`,
	}, " && ")
}

// forkSnapshotConfigChanges returns docker commit --change entries for a fork snapshot.
//
// It clears labels and runtime ENV inherited from the source container image so
// launchContainer can apply the fork's requested metadata and capabilities, then
// stamps imageTypeLabelKey so the snapshot stays discoverable for pruning even
// after it is untagged.
func forkSnapshotConfigChanges(labelCSV string) []string {
	labels := strings.Fields(labelCSV)
	changes := make([]string, 0, len(labels)+len(forkSnapshotEnvKeys)+1)
	for _, key := range labels {
		changes = append(changes, "LABEL "+key+"=")
	}
	for _, key := range forkSnapshotEnvKeys {
		changes = append(changes, "ENV "+key+"=")
	}
	// Applied after the clearing pass above so it wins over an inherited
	// md.image_type (the source runs on a specialized image).
	changes = append(changes, "LABEL md.image_type="+imageTypeForkSnapshot)
	return changes
}

// DiskUsage returns the writable container layer size in bytes via
// docker inspect --size. Works for both running and stopped containers.
func (c *Container) DiskUsage(ctx context.Context) (int64, error) {
	sz, err := c.Runtime.DiskUsage(ctx, c.Name)
	if err != nil {
		return -1, fmt.Errorf("inspecting container %s: %w", c.Name, err)
	}
	return sz, nil
}

// Status returns the Docker container state (e.g. "running", "exited", "").
// Returns empty string when the container does not exist.
func (c *Container) Status(ctx context.Context) string {
	out, err := c.Runtime.Run(ctx, "", "inspect", "--format", "{{.State.Status}}", c.Name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Inspect returns detailed observed runtime configuration for the container.
func (c *Container) Inspect(ctx context.Context) (*InspectInfo, error) {
	info, err := c.Runtime.InspectInfo(ctx, c.Name)
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", c.Name, err)
	}
	return inspectInfoFromRuntime(info), nil
}

// SudoPassword retrieves the random sudo password set at container startup,
// or "" if no password was configured.
//
// For containers created by Launch, the password is cached in-memory.
// For containers loaded from List (e.g. md sudo-password), it falls back
// to reading the md.sudo-password Docker label.
func (c *Container) SudoPassword(ctx context.Context) (string, error) {
	if c.sudoPassword != "" {
		return c.sudoPassword, nil
	}
	out, err := c.Runtime.Run(ctx, "", "inspect", "--format", `{{index .Config.Labels "md.sudo-password"}}`, c.Name)
	if err != nil {
		return "", err
	}
	c.sudoPassword = strings.TrimSpace(out)
	return c.sudoPassword, nil
}

// TailscaleFQDN returns the Tailscale FQDN for the container, or "" if unavailable.
func (c *Container) TailscaleFQDN(ctx context.Context) string {
	if !c.Tailscale || c.State != "running" {
		return ""
	}
	statusJSON, err := c.Runtime.Run(ctx, "", "exec", c.Name, "tailscale", "status", "--json")
	if err != nil {
		c.Logger.Log(ctx, slog.LevelDebug, "tailscale status failed", "err", err)
		return ""
	}
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		c.Logger.Log(ctx, slog.LevelDebug, "tailscale status JSON parse failed", "err", err)
		return ""
	}
	fqdn := strings.TrimRight(status.Self.DNSName, ".")
	if fqdn == "" {
		c.Logger.Log(ctx, slog.LevelDebug, "tailscale FQDN empty")
	}
	return fqdn
}

// SyncDefaultBranch mirrors the host's cached remote branches for Repos[repoIdx]
// into the container. The historical name is retained for API compatibility.
func (c *Container) SyncDefaultBranch(ctx context.Context, repoIdx int) error {
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	return c.syncRepoRefs(ctx, &c.Repos[repoIdx])
}

func (c *Container) syncRepoRefs(ctx context.Context, r *Repo) error {
	refspecs, err := r.containerSyncRefspecs(ctx, c.Logger)
	if err != nil {
		return fmt.Errorf("selecting refs from repository %q for container %q: %w", r.GitRoot, c.Name, err)
	}
	if err := c.pushContainerRefs(ctx, r, refspecs); err != nil {
		return fmt.Errorf("syncing refs %q from repository %q to container %q: %w", refspecs, r.GitRoot, c.Name, err)
	}
	return nil
}

func (c *Container) containerBranchUpstream(ctx context.Context, repoIdx int, branch string) (branchUpstreamSpec, error) {
	r := &c.Repos[repoIdx]
	prefix := "branch." + branch
	command := "cd " + shellQuote(r.ContainerPath) + " || exit $?; " +
		"remote=$(git config --local --get " + shellQuote(prefix+".remote") + "); status=$?; " +
		`if [ "$status" -eq 1 ]; then printf missing; exit 0; fi; ` +
		`if [ "$status" -ne 0 ]; then exit "$status"; fi; ` +
		"merge=$(git config --local --get " + shellQuote(prefix+".merge") + "); status=$?; " +
		`if [ "$status" -eq 1 ]; then printf missing; exit 0; fi; ` +
		`if [ "$status" -ne 0 ]; then exit "$status"; fi; ` +
		`printf 'configured\0%s\0%s' "$remote" "$merge"`
	out, err := c.runCmd(ctx, "", c.SSHCommand(nil, command))
	if err != nil {
		return branchUpstreamSpec{}, fmt.Errorf("inspecting upstream for branch %q in repository %q via SSH to container %q: %w", branch, r.ContainerPath, c.Name, err)
	}
	if out == "missing" {
		return branchUpstreamSpec{}, fmt.Errorf("container branch %q has no configured upstream; run md pull or md push to synchronize it", branch)
	}
	fields := strings.Split(out, "\x00")
	if len(fields) != 3 || fields[0] != "configured" || fields[1] == "" {
		return branchUpstreamSpec{}, fmt.Errorf("invalid container upstream for mapped branch %q: %q; run md pull or md push to synchronize it", branch, out)
	}
	upstreamBranch, ok := strings.CutPrefix(fields[2], "refs/heads/")
	if !ok || upstreamBranch == "" {
		return branchUpstreamSpec{}, fmt.Errorf("invalid container upstream ref for mapped branch %q: %q; run md pull or md push to synchronize it", branch, fields[2])
	}
	return branchUpstreamSpec{remote: fields[1], branch: upstreamBranch}, nil
}

func (c *Container) validateContainerBranchUpstreams(ctx context.Context, repoIdx int) error {
	r := &c.Repos[repoIdx]
	for _, branch := range r.Branches {
		hostRemote, hostBranch, err := r.requiredMappedBranchUpstream(ctx, c.Logger, branch)
		if err != nil {
			return err
		}
		containerUpstream, err := c.containerBranchUpstream(ctx, repoIdx, branch)
		if err != nil {
			return err
		}
		if containerUpstream.remote != hostRemote || containerUpstream.branch != hostBranch {
			return fmt.Errorf("mapped branch %q upstream changed from %q in the container to %q on the host; run md pull or md push to synchronize it", branch, branchUpstreamDisplay(containerUpstream.remote, containerUpstream.branch), branchUpstreamDisplay(hostRemote, hostBranch))
		}
	}
	return nil
}

func branchUpstreamDisplay(remote, branch string) string {
	if remote == "." {
		return branch + " (local)"
	}
	return remote + "/" + branch
}

func (c *Container) restorePullCheckout(ctx context.Context, stdout, stderr io.Writer, g *git.Checkout, originalBranch, originalCommit string) error {
	finalBranch, err := g.RunGit(ctx, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("reading final branch: %w", err)
	}
	if originalBranch != "" {
		if finalBranch != originalBranch {
			if err := c.runCmdOut(ctx, g.Root, []string{"git", "switch", "-q", originalBranch}, stdout, stderr); err != nil {
				return fmt.Errorf("restoring original branch %s: %w", originalBranch, err)
			}
		}
		return nil
	}
	finalCommit, err := g.RevParse(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("reading final HEAD: %w", err)
	}
	if finalBranch != "" || finalCommit != originalCommit {
		if err := c.runCmdOut(ctx, g.Root, []string{"git", "switch", "-q", "--detach", originalCommit}, stdout, stderr); err != nil {
			return fmt.Errorf("restoring detached HEAD %s: %w", originalCommit, err)
		}
	}
	return nil
}

func (c *Container) recoverPullCheckout(ctx context.Context, stdout, stderr io.Writer, g *git.Checkout, originalBranch, originalCommit string) error {
	rebaseActive, err := gitRebaseInProgress(ctx, g)
	if err != nil {
		return errors.Join(fmt.Errorf("checking for an interrupted rebase: %w", err), c.restorePullCheckout(ctx, stdout, stderr, g, originalBranch, originalCommit))
	}
	if rebaseActive {
		return nil
	}
	return c.restorePullCheckout(ctx, stdout, stderr, g, originalBranch, originalCommit)
}

func (c *Container) untagImage(ctx context.Context, image string) error {
	if err := c.Runtime.UntagImage(ctx, image); err != nil {
		return fmt.Errorf("untagging image %s: %w", image, err)
	}
	return nil
}

// revokeSudo removes sudo access from a forked container.
//
// A fork created from a sudo-enabled snapshot can inherit /etc/group and the
// user's password hash. Revocation makes an explicit non-sudo fork match its
// requested capabilities before the SSH readiness probe runs.
func (c *Container) revokeSudo(ctx context.Context) error {
	out, err := c.Runtime.Run(ctx, "", "exec", "--user", "0:0", c.Name, "bash", "-lc", revokeSudoCommand)
	if err != nil {
		return fmt.Errorf("revoking sudo in %s: %w: %s", c.Name, err, out)
	}
	return nil
}

// fillFromInspect parses docker/podman inspect JSON output into c.
//
// Both Docker and Podman inspect return a JSON array, even for a single
// container.
func (c *Container) fillFromInspect(ctx context.Context, data []byte) error {
	raw, err := containers.ParseInspectContainer(data)
	if err != nil {
		return err
	}
	c.Name = raw.Name
	c.State = raw.State
	c.Labels = maps.Clone(raw.Labels)
	c.SSHPort = raw.SSHPort
	c.VNCPort = raw.VNCPort
	c.CreatedAt = raw.CreatedAt
	return c.loadMDLabels(ctx, raw.Labels)
}

func (c *Container) loadMDLabels(ctx context.Context, labels map[string]string) error {
	for k, v := range labels {
		switch k {
		case "md.repos":
			if data, err := base64.StdEncoding.DecodeString(v); err == nil {
				if err := json.Unmarshal(data, &c.Repos); err != nil {
					c.Logger.Log(ctx, slog.LevelWarn, "failed to unmarshal repos label", "err", err)
				}
				c.migrateRepoRemotes(ctx)
				for i := range c.Repos {
					if err := c.Repos[i].Validate(); err != nil {
						return fmt.Errorf("inspect repos[%d]: %w", i, err)
					}
				}
			}
		case "md.display":
			c.Display = v == "1"
		case "md.tailscale":
			c.Tailscale = v == "1"
		case "md.usb":
			c.USB = v == "1"
		case "md.sudo":
			c.Sudo = v == "1"
		}
	}
	return nil
}

// migrateRepoRemotes reconstructs Remotes for container labels created before
// Repo.Remotes was persisted. It reads the host checkout's configured remotes,
// excluding synthetic transport remotes while retaining a recorded default that
// is no longer configured. Migration is in-memory only; discovery failures are
// logged so container inspection can still proceed.
func (c *Container) migrateRepoRemotes(ctx context.Context) {
	for i := range c.Repos {
		r := &c.Repos[i]
		r.populateContainerPath()
		r.ContainerPath = ResolveContainerPath(r.ContainerPath)
		if len(r.Remotes) == 0 {
			if err := r.resolveRemotes(ctx, c.Logger, c.Name); err != nil {
				c.Logger.WarnContext(ctx, "failed to migrate repo remotes", "repo", r.GitRoot, "err", err)
			}
			if r.DefaultRemote != "" && !slices.Contains(r.Remotes, r.DefaultRemote) {
				r.Remotes = append(r.Remotes, r.DefaultRemote)
			}
		}
	}
}

func (c *Container) configureContainerRemotes(ctx context.Context, stdout, stderr io.Writer, repoIdx int, refreshUpstreams bool, postCommands ...string) error {
	// postCommands run after the remote config in the same remote shell. Push,
	// Pull, and provisioning use this to express the full ordered git transition
	// at one call site.
	commands, err := c.containerGitConfigCommands(ctx, repoIdx, refreshUpstreams, postCommands...)
	if err != nil {
		return err
	}
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, strings.Join(commands, " && ")), stdout, stderr); err != nil {
		r := &c.Repos[repoIdx]
		return fmt.Errorf("configuring remotes for %s: %w", r.ContainerPath, err)
	}
	return nil
}

func (c *Container) containerGitConfigCommands(ctx context.Context, repoIdx int, refreshUpstreams bool, postCommands ...string) ([]string, error) {
	r := &c.Repos[repoIdx]
	configs, err := r.containerRemoteConfigs(ctx, c.Logger)
	if err != nil {
		return nil, fmt.Errorf("reading host remotes for %s: %w", r.GitRoot, err)
	}
	identity, err := r.gitIdentity(ctx, c.Logger)
	if err != nil {
		return nil, err
	}
	hooksPath, err := r.containerHooksPath(ctx, c.Logger)
	if err != nil {
		return nil, err
	}
	var upstreamCommands []string
	if refreshUpstreams {
		upstreamCommands, err = r.containerBranchUpstreamCommands(ctx, c.Logger)
		if err != nil {
			return nil, err
		}
	}
	commands := containerRemoteConfigCommands(r, configs)
	commands = slices.Insert(commands, 1, gitIdentityCommands("--local", identity)...)
	if hooksPath != "" {
		commands = append(commands, "git config --local --replace-all core.hooksPath "+shellQuote(hooksPath))
	}
	commands = append(commands, postCommands...)
	return append(commands, upstreamCommands...), nil
}

func containerRemoteConfigCommands(r *Repo, configs []containerRemoteConfig) []string {
	commands := []string{"cd " + shellQuote(r.ContainerPath)}
	for _, config := range configs {
		remoteConfigPrefix := "remote." + config.name
		if config.url != "" {
			commands = append(commands, "git config --replace-all "+shellQuote(remoteConfigPrefix+".url")+" "+shellQuote(config.url))
		}
		commands = append(commands, "git config --replace-all "+shellQuote(remoteConfigPrefix+".fetch")+" "+shellQuote("+refs/heads/*:"+remoteTrackingRef(config.name, "*")))
		if config.configured {
			commands = append(commands, "(git config --unset-all "+shellQuote(remoteConfigPrefix+".pushurl")+" >/dev/null 2>&1 || true)")
			if config.pushURL != "" && config.pushURL != config.url {
				commands = append(commands, "git config --replace-all "+shellQuote(remoteConfigPrefix+".pushurl")+" "+shellQuote(config.pushURL))
			}
		}
	}
	return commands
}

func gitIdentityCommands(scope string, identity gitIdentity) []string {
	commands := make([]string, 0, 2)
	for _, setting := range []struct {
		key   string
		value string
	}{
		{key: "user.email", value: identity.email},
		{key: "user.name", value: identity.name},
	} {
		if setting.value == "" {
			commands = append(commands, "(git config "+scope+" --unset-all "+setting.key+" >/dev/null 2>&1 || true)")
		} else {
			commands = append(commands, "git config "+scope+" --replace-all "+setting.key+" "+shellQuote(setting.value))
		}
	}
	return commands
}

// configureGlobalGitIdentity sets the host user's global identity in the
// container before agents can commit in bind-mounted repositories.
func (c *Container) configureGlobalGitIdentity(ctx context.Context, stdout, stderr io.Writer) error {
	identity, err := c.globalGitIdentity(ctx)
	if err != nil {
		return err
	}
	commands := gitIdentityCommands("--global", identity)
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, strings.Join(commands, " && ")), stdout, stderr); err != nil {
		return fmt.Errorf("configuring global Git identity: %w", err)
	}
	return nil
}

func (c *Container) pushContainerRefs(ctx context.Context, r *Repo, refspecs []string) error {
	if len(refspecs) == 0 {
		return nil
	}
	var stderr bytes.Buffer
	if err := c.pushRefspecs(ctx, r.GitRoot, c.Name, refspecs, true, io.Discard, &stderr); err != nil {
		return commandErrorWithStderr("pushing refs", err, stderr.String())
	}
	return nil
}

// pushRefspecs pushes refspecs in command-line-size-bounded batches. It
// disables repository hooks and implicit follow-tags behavior, and optionally
// forces updates. A failure can leave earlier batches applied; an empty
// refspec list is a no-op.
func (c *Container) pushRefspecs(ctx context.Context, gitRoot, remote string, refspecs []string, force bool, stdout, stderr io.Writer) error {
	for start := 0; start < len(refspecs); {
		end := refspecBatchEnd(refspecs, start)
		args := make([]string, 0, 8+end-start)
		// These pushes replicate refs into a task container. They must not run
		// user-configured pre-push hooks, which are intended for developer
		// publishes and can require unavailable host-specific dependencies.
		args = append(args, "git", "push", "-q", "--no-verify", "--no-follow-tags")
		if force {
			args = append(args, "-f")
		}
		args = append(args, remote)
		args = append(args, refspecs[start:end]...)
		if err := c.runCmdOut(ctx, gitRoot, args, stdout, stderr); err != nil {
			return err
		}
		start = end
	}
	return nil
}

// refspecBatchEnd returns the exclusive end of the batch beginning at start.
// It respects maxPushRefspecBytes while always including at least one refspec.
func refspecBatchEnd(refspecs []string, start int) int {
	end := start
	size := 0
	for end < len(refspecs) && (end == start || size+len(refspecs[end])+1 <= maxPushRefspecBytes) {
		size += len(refspecs[end]) + 1
		end++
	}
	return end
}

func appendUniqueRefspecs(refspecs []string, more ...string) []string {
	// Deduplicate by destination. Two sources can legitimately target the same
	// container ref when a mapped branch is also the default/tracked branch.
	seen := make(map[string]struct{}, len(refspecs)+len(more))
	for _, refspec := range refspecs {
		seen[refspecDestination(refspec)] = struct{}{}
	}
	for _, refspec := range more {
		destination := refspecDestination(refspec)
		if _, ok := seen[destination]; ok {
			continue
		}
		seen[destination] = struct{}{}
		refspecs = append(refspecs, refspec)
	}
	return refspecs
}

func refspecDestination(refspec string) string {
	_, destination, ok := strings.Cut(refspec, ":")
	if !ok {
		return refspec
	}
	return destination
}

// pushMappedBranchRefs pushes all mapped host branches and the refs that seed
// them inside the container into the container remote.
//
// It returns one base per mapped branch for configuring the container working
// tree.
func (c *Container) pushMappedBranchRefs(ctx context.Context, stdout, stderr io.Writer, r *Repo) ([]containerBranchBase, error) {
	bases := make([]containerBranchBase, len(r.Branches))
	refspecs := make([]string, 0, len(r.Branches))
	for i, b := range r.Branches {
		base, err := r.resolveContainerBranchBase(ctx, c.Logger, b)
		if err != nil {
			return nil, fmt.Errorf("resolve branch base for %s: %w", b, err)
		}
		bases[i] = base
		refspecs = appendUniqueRefspecs(refspecs, base.source+":"+base.destination)
	}
	syncRefspecs, err := r.containerSyncRefspecs(ctx, c.Logger)
	if err != nil {
		return nil, err
	}
	refspecs = appendUniqueRefspecs(refspecs, syncRefspecs...)

	if err := c.pushRefspecs(ctx, r.GitRoot, c.Name, refspecs, true, stdout, stderr); err != nil {
		return nil, fmt.Errorf("push mapped branches: %w", err)
	}
	return bases, nil
}

// containerBranchSetupCommands returns shell commands that create/reset mapped
// branches inside the container from their selected base refs and configure each
// branch to track the same upstream as its host branch.
func containerBranchSetupCommands(bases []containerBranchBase) []string {
	commands := make([]string, 0, 6*len(bases))
	for i, base := range bases {
		branch := shellQuote(base.branch)
		baseRef := shellQuote(base.ref)
		if i == 0 {
			commands = append(commands, "git switch -q -C "+branch+" "+baseRef)
		} else {
			commands = append(commands, "git branch -q -f "+branch+" "+baseRef)
		}
		commands = append(commands, branchTrackingCommands(base.branch, branchUpstreamSpec{
			remote:     base.upstreamRemote,
			branch:     base.upstreamBranch,
			pushRemote: base.pushRemote,
		})...)
		if base.ref == base.destination && strings.HasPrefix(base.destination, containerIncomingRefPrefix) {
			commands = append(commands, "git update-ref -d "+baseRef)
		}
	}
	return commands
}

// containerSyncRefspec returns the refspec that records source as the sync
// point of branch inside the container, which is the base "md diff" reports
// from. Source must resolve to a commit the host already has.
func containerSyncRefspec(source, branch string) string {
	return "+" + source + ":" + containerSyncRefPrefix + branch
}

// branchSyncRefspecs returns one refspec per branch pair, recording each source
// branch's host commit as the integration point of the destination branch
// created from it. The two slices pair by position; planFork builds them that
// way.
func branchSyncRefspecs(sourceBranches, destBranches []string) []string {
	refspecs := make([]string, 0, len(sourceBranches))
	for i, b := range sourceBranches {
		refspecs = append(refspecs, containerSyncRefspec("refs/heads/"+b, destBranches[i]))
	}
	return refspecs
}

// recordHostBranchSyncPoints records the mapped host branch tips as the bases
// reported by md diff. Call it only after the container branches have been
// reset successfully, so a failed reset leaves the previous bases in place.
func (c *Container) recordHostBranchSyncPoints(ctx context.Context, r *Repo) error {
	if err := c.pushContainerRefs(ctx, r, branchSyncRefspecs(r.Branches, r.Branches)); err != nil {
		return fmt.Errorf("recording integration points for %s: %w", r.ContainerPath, err)
	}
	return nil
}

// recordFetchedBranchSyncPoints acknowledges fetched container commits only
// after Pull has integrated every mapped branch into the host checkout.
func (c *Container) recordFetchedBranchSyncPoints(ctx context.Context, r *Repo, fetched []FetchedBranch) error {
	refspecs := make([]string, 0, len(fetched))
	for _, f := range fetched {
		refspecs = append(refspecs, containerSyncRefspec(f.CommitHash, f.BranchName))
	}
	if err := c.pushContainerRefs(ctx, r, refspecs); err != nil {
		return fmt.Errorf("recording integration points for %s: %w", r.ContainerPath, err)
	}
	return nil
}

// forkSyncPointCommands records the fork's committed branch tips as fresh
// integration points. Fork fetched those tips to the host before running these
// commands, so inherited committed work is already present on both sides while
// inherited working-tree changes remain visible to md diff.
// planFork guarantees oldPrimary is absent from branches, so the final
// deletion cannot remove an integration point recorded above.
func forkSyncPointCommands(oldPrimary string, branches []string) []string {
	commands := make([]string, 0, len(branches)+1)
	for _, branch := range branches {
		commands = append(commands, "git update-ref "+shellQuote(containerSyncRefPrefix+branch)+" "+shellQuote("refs/heads/"+branch))
	}
	commands = append(commands, "git update-ref -d "+shellQuote(containerSyncRefPrefix+oldPrimary))
	return commands
}

func branchUpstreamCommands(branch, remote, upstreamBranch string) []string {
	prefix := "branch." + branch
	return []string{
		"git config --replace-all " + shellQuote(prefix+".remote") + " " + shellQuote(remote),
		"git config --replace-all " + shellQuote(prefix+".merge") + " " + shellQuote("refs/heads/"+upstreamBranch),
	}
}

func branchTrackingCommands(branch string, spec branchUpstreamSpec) []string {
	commands := branchUpstreamCommands(branch, spec.remote, spec.branch)
	commands = append(commands, "(git config --unset-all "+shellQuote("branch."+branch+".pushRemote")+" >/dev/null 2>&1 || true)")
	if spec.pushRemote != "" {
		commands = append(commands, "git config --replace-all "+shellQuote("branch."+branch+".pushRemote")+" "+shellQuote(spec.pushRemote))
	}
	return commands
}

// backupContainerBranchesCommands returns shell commands that preserve the
// current container HEAD and every mapped container branch before a destructive
// push resets them from the host.
func backupContainerBranchesCommands(branches []string, backupBranch string) []string {
	commands := make([]string, 0, 1+2*len(branches))
	commands = append(commands, "git branch -q -f "+shellQuote(backupBranch)+" HEAD")
	for i, b := range branches {
		branch := shellQuote("refs/heads/" + b)
		backup := shellQuote(backupBranch + "-" + strconv.Itoa(i) + "-" + sanitizeDockerName(b))
		commands = append(commands,
			"git rev-parse --verify -q "+branch+" >/dev/null",
			"git branch -q -f "+backup+" "+branch,
		)
	}
	return commands
}

type containerBranchBase struct {
	branch         string
	source         string
	ref            string
	upstreamRemote string
	upstreamBranch string
	destination    string
	pushRemote     string
}

type containerRemoteConfig struct {
	name       string
	url        string
	pushURL    string
	configured bool
}

type gitIdentity struct {
	name  string
	email string
}

func defaultBranchPushSource(ctx context.Context, r *Repo) (src string, ok bool, err error) {
	g := &git.Checkout{Root: r.GitRoot}
	remoteRef := remoteTrackingRef(r.DefaultRemote, r.DefaultBranch)
	ok, err = g.RefExists(ctx, remoteRef)
	if err != nil {
		return "", false, err
	}
	if ok {
		return remoteRef, true, nil
	}
	localRef := "refs/heads/" + r.DefaultBranch
	ok, err = g.RefExists(ctx, localRef)
	if err != nil {
		return "", false, err
	}
	if ok {
		return localRef, true, nil
	}
	return "", false, nil
}

func remoteTrackingRef(remote, branch string) string {
	return "refs/remotes/" + remote + "/" + branch
}

func (c *Container) prepareTailscaleAuthKey(ctx context.Context, stdout io.Writer, opts *StartOpts) {
	if !opts.Tailscale || opts.TailscaleAuthKey != "" {
		return
	}
	key, err := generateTailscaleAuthKey(ctx, c.TailscaleAPIKey)
	if err != nil {
		if !opts.Quiet {
			_, _ = fmt.Fprintf(stdout, "- Could not generate Tailscale auth key (%v), will use browser auth\n", err)
		}
		return
	}
	opts.TailscaleAuthKey = key
	c.tailscaleEphemeral = true
}

func (c *Container) tailscaleDeviceID(ctx context.Context) (string, error) {
	statusJSON, statusErr := c.Runtime.Run(ctx, "", "exec", c.Name, "tailscale", "status", "--json")
	if statusErr == nil {
		deviceID, err := tailscaleDeviceIDFromStatus(statusJSON)
		if err == nil && deviceID != "" {
			return deviceID, nil
		}
		if err != nil {
			statusErr = err
		}
	}

	deviceID, fileErr := c.readContainerFile(ctx, tailscaleDeviceIDPath)
	if fileErr == nil {
		return strings.TrimSpace(deviceID), nil
	}
	if errors.Is(fileErr, fs.ErrNotExist) {
		return "", nil
	}
	return "", errors.Join(statusErr, fileErr)
}

func tailscaleDeviceIDFromStatus(statusJSON string) (string, error) {
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return "", err
	}
	return strings.TrimSpace(status.Self.ID), nil
}

func (c *Container) readContainerFile(ctx context.Context, containerPath string) (content string, err error) {
	tmpDir, err2 := os.MkdirTemp("", "md-container-file-*")
	if err2 != nil {
		return "", err2
	}
	defer func() {
		if err2 := os.RemoveAll(tmpDir); err2 != nil && !os.IsNotExist(err2) {
			err = errors.Join(err, err2)
		}
	}()

	containerDir := path.Dir(containerPath)
	// Copy the parent directory so a missing file becomes a typed fs.ErrNotExist
	// from os.ReadFile instead of runtime-specific command text.
	if _, err := c.Runtime.Run(ctx, "", "cp", c.Name+":"+containerDir+"/.", tmpDir); err != nil {
		return "", err
	}
	dst := filepath.Join(tmpDir, path.Base(containerPath))
	data, err2 := os.ReadFile(dst) // #nosec G304 -- dst is a private temp file populated by docker cp.
	if err2 != nil {
		return "", err2
	}
	return string(data), nil
}

// pullBranches integrates mapped branches from the container's remote-tracking
// refs into the host's local branches. previousTips are the tracking refs from
// before the fetch; they identify amended container commits that replace,
// rather than extend, host history.
func (c *Container) pullBranches(ctx context.Context, stdout, stderr io.Writer, g *git.Checkout, branches []string, previousTips map[string]string) error {
	for _, branch := range branches {
		if err := c.pullBranch(ctx, stdout, stderr, g, branch, previousTips[branch]); err != nil {
			return fmt.Errorf("integrating branch %s: %w", branch, err)
		}
	}
	return nil
}

func (c *Container) pullBranch(ctx context.Context, stdout, stderr io.Writer, g *git.Checkout, branch, previousTip string) error {
	localRef := "refs/heads/" + branch
	remoteRef := remoteTrackingRef(c.Name, branch)
	run := func(operation string, args ...string) error {
		if err := c.runCmdOut(ctx, g.Root, append([]string{"git"}, args...), stdout, stderr); err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}
		return nil
	}

	exists, err := g.RefExists(ctx, localRef)
	if err != nil {
		return err
	}
	if !exists {
		return run("creating local branch "+branch, "update-ref", localRef, remoteRef)
	}
	currentBranch, err := g.RunGit(ctx, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("reading current branch: %w", err)
	}
	if previousTip != "" {
		extendsPreviousTip, err := gitIsAncestor(ctx, g, previousTip, remoteRef)
		if err != nil {
			return fmt.Errorf("comparing previous and fetched container tips: %w", err)
		}
		if !extendsPreviousTip {
			localTip, err := g.RevParse(ctx, localRef)
			if err != nil {
				return fmt.Errorf("reading local tip: %w", err)
			}
			if localTip == previousTip {
				if currentBranch == branch {
					return run("resetting branch "+branch+" to rewritten container history", "reset", "--hard", "-q", remoteRef)
				}
				return run("updating branch "+branch+" to rewritten container history", "update-ref", localRef, remoteRef, previousTip)
			}
			localExtendsPreviousTip, err := gitIsAncestor(ctx, g, previousTip, localRef)
			if err != nil {
				return fmt.Errorf("comparing previous container and local tips: %w", err)
			}
			if localExtendsPreviousTip {
				if err := run("switching to branch "+branch, "switch", "-q", branch); err != nil {
					return err
				}
				return run("rebasing branch "+branch+" onto rewritten container history", "rebase", "-q", "--onto", remoteRef, previousTip)
			}
		}
	}
	if currentBranch == branch {
		return run("rebasing branch "+branch+" onto "+remoteRef, "rebase", "-q", remoteRef)
	}
	fastForward, err := gitIsAncestor(ctx, g, localRef, remoteRef)
	if err != nil {
		return fmt.Errorf("checking for fast-forward: %w", err)
	}
	if fastForward {
		return run("fast-forwarding branch "+branch, "update-ref", localRef, remoteRef)
	}
	if err := run("switching to branch "+branch, "switch", "-q", branch); err != nil {
		return err
	}
	return run("rebasing branch "+branch+" onto "+remoteRef, "rebase", "-q", remoteRef)
}

func gitIsAncestor(ctx context.Context, g *git.Checkout, ancestor, descendant string) (bool, error) {
	if _, err := g.RunGit(ctx, "merge-base", "--is-ancestor", ancestor, descendant); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func gitRebaseInProgress(ctx context.Context, g *git.Checkout) (bool, error) {
	for _, name := range []string{"rebase-apply", "rebase-merge"} {
		stateDir, err := g.RunGit(ctx, "rev-parse", "--git-path", name)
		if err != nil {
			return false, err
		}
		if !filepath.IsAbs(stateDir) {
			stateDir = filepath.Join(g.Root, stateDir)
		}
		if _, err := os.Stat(stateDir); err == nil {
			return true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// waitForSSH runs a trivial SSH command in a retry loop until it succeeds or
// the deadline is exceeded. This confirms SSH is fully operational after the
// TCP socket opens (sshd may need a few more milliseconds to accept auth).
func (c *Container) waitForSSH(ctx context.Context, deadline time.Time) error {
	var lastErr error
	sshArgs := c.SSHCommand(nil, "true")
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "cmd", sshArgs)
	for {
		cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Env = c.commandEnv()
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			lastErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for SSH on %s: %w", c.Name, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gatherGitMetadata runs SSH commands to collect branch, stat, and log from
// the container. This data is always small.
func (c *Container) gatherGitMetadata(ctx context.Context, r *Repo) string {
	repo := shellQuote(r.ContainerPath)
	cmd := "cd " + repo + " && " + gitBaseRefCommand() + " && echo '=== Branch ===' && git rev-parse --abbrev-ref HEAD && echo && echo '=== Files Changed ===' && git -c core.quotePath=true -c color.ui=false diff --no-color --no-ext-diff --no-textconv --stat --stat-width=80 --stat-name-width=60 --stat-count=100 --cached HEAD -- . && echo && echo '=== Recent Commits ===' && git -c color.ui=false log -5 --no-color --format='%H %s' \"$base_ref\" -- ."
	out, _ := c.runCmd(ctx, "", c.SSHCommand(nil, cmd))
	return out
}

// gatherGitDiff runs SSH to get the full patience diff from the container.
// Explicit formatting flags keep repository and user Git configuration from
// changing the patch consumed by the commit-message generator.
func (c *Container) gatherGitDiff(ctx context.Context, r *Repo) string {
	repo := shellQuote(r.ContainerPath)
	cmd := "cd " + repo + " && git -c core.quotePath=true -c color.ui=false diff --no-color --no-ext-diff --no-textconv --patience --find-renames=50% --src-prefix=a/ --dst-prefix=b/ --output-indicator-new=+ --output-indicator-old=- --output-indicator-context=' ' --submodule=short --no-relative --unified=10 --cached HEAD -- ."
	out, _ := c.runCmd(ctx, "", c.SSHCommand(nil, cmd))
	return out
}

type containerStatePolicy uint8

const (
	containerMustBeRunning containerStatePolicy = iota
	containerMayBeStopped
)

func (c *Container) checkContainerState(ctx context.Context, policy containerStatePolicy) error {
	inspected, err := c.Runtime.InspectContainer(ctx, c.Name)
	if err != nil {
		return fmt.Errorf("inspecting container %q with %s: %w", c.Name, c.Runtime.Name(), err)
	}
	state := inspected.State
	if state != "" && state != "running" && (policy != containerMayBeStopped || state != "exited") {
		if policy == containerMayBeStopped {
			return fmt.Errorf("Container %s is %s. Fork requires a running or stopped container", c.Name, state)
		}
		return fmt.Errorf("Container %s is stopped. Restart it with: md start", c.Name)
	}
	var issues []string
	for i := range c.Repos {
		gitRoot := c.Repos[i].GitRoot
		if _, err := c.runCmd(ctx, gitRoot, []string{"git", "remote", "get-url", c.Name}); err != nil {
			issues = append(issues, fmt.Sprintf("Git remote is missing from %q", gitRoot))
		}
	}
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	_, sshErr := os.Stat(filepath.Join(sshConfigDir, c.Name+".conf"))
	sshExists := sshErr == nil

	if !sshExists {
		issues = append(issues, "SSH config is missing")
	}
	if len(issues) > 0 {
		return fmt.Errorf("inconsistent state detected for %s:\n  - %s\nConsider running 'md purge' to clean up, then 'md start' to restart",
			c.Name, strings.Join(issues, "\n  - "))
	}
	return nil
}

func (c *Container) refreshRuntimeFields(ctx context.Context) error {
	raw, err := c.Runtime.InspectContainer(ctx, c.Name)
	if err != nil {
		return err
	}
	c.Name = raw.Name
	c.State = raw.State
	c.Labels = maps.Clone(raw.Labels)
	c.SSHPort = raw.SSHPort
	c.VNCPort = raw.VNCPort
	c.CreatedAt = raw.CreatedAt
	return c.loadMDLabels(ctx, raw.Labels)
}

// ensureImage checks whether the user image needs rebuilding and, if so,
// builds it. Returns the immutable image ID so concurrent tag updates do not
// affect the container launch. The build is serialized via Client.buildMu.
func (c *Container) ensureImage(ctx context.Context, stdout, stderr io.Writer, baseImage, platform string, caches []CacheMount, quiet bool) (string, error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	p := Platform(platform).Resolve()
	if err := p.Validate(); err != nil {
		return "", err
	}
	if err := validateCacheMounts(caches); err != nil {
		return "", err
	}
	platform = p.String()
	imageName := userImageName(baseImage, activeCacheKey(caches, c.Home), platform)
	if !c.imageBuildNeeded(ctx, imageName, baseImage, platform, caches) {
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "- Docker image %s is up to date, skipping build.\n", imageName)
		}
		return c.imageID(ctx, imageName)
	}
	imageID, err := c.buildSpecializedImage(ctx, stdout, stderr, imageName, baseImage, platform, caches, agentContainerPaths(), quiet)
	if err != nil {
		return "", err
	}
	c.invalidateImageBuildCache()
	return imageID, nil
}

// tagRefspecs returns one source-to-destination refspec for each tag selected
// from the newline-delimited output of `git tag --list`. Matching uses Go
// regular-expression MatchString semantics; an empty expression selects none.
func tagRefspecs(tagRegexp, tagOutput string) ([]string, error) {
	if tagRegexp == "" || tagOutput == "" {
		return nil, nil
	}
	pattern, err := regexp.Compile(tagRegexp)
	if err != nil {
		return nil, fmt.Errorf("compile tag regexp %q: %w", tagRegexp, err)
	}
	tags := strings.Split(tagOutput, "\n")
	refspecs := make([]string, 0, len(tags))
	for _, tag := range tags {
		if pattern.MatchString(tag) {
			ref := "refs/tags/" + tag
			refspecs = append(refspecs, ref+":"+ref)
		}
	}
	return refspecs, nil
}

// runCmd executes a command, captures its output, and returns (stdout, error).
// If dir is non-empty, the command runs in that directory.
func (c *Container) runCmd(ctx context.Context, dir string, args []string) (string, error) {
	// Command arguments are redacted before logging.
	// codeql[go/clear-text-logging]
	c.Logger.Log(ctx, slog.LevelDebug, "exec", "cmd", containers.RedactCommandArgsForLog(args))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = c.commandEnv("LANG=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = commandErrorWithStderr("", err, stderr.String())
	}
	return strings.TrimSpace(string(out)), err
}

// runCmdOut executes a command, directing its stdout and stderr to the given writers.
// If dir is non-empty, the command runs in that directory.
func (c *Container) runCmdOut(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) error {
	// Command arguments are redacted before logging.
	// codeql[go/clear-text-logging]
	c.Logger.Log(ctx, slog.LevelDebug, "exec", "cmd", containers.RedactCommandArgsForLog(args))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = c.commandEnv("LANG=C")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// runGitDir runs a git command with GIT_DIR and GIT_WORK_TREE explicitly set,
// fully decoupling Git from the repository's core.worktree configuration.
func (c *Container) runGitDir(ctx context.Context, dir, gitDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = c.commandEnv("GIT_DIR="+gitDir, "GIT_WORK_TREE="+dir, "LANG=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// pushSubmodules transfers submodule bare repos from hostGitRoot into the
// container at containerRepoPath and initializes them at all nesting depths
// without requiring network access. containerRepoPath is the absolute path
// inside the container (e.g. /home/user/src/myrepo).
//
// Returns nil when no submodules are declared or when submodules are declared
// but not yet cloned on the host (uninitialized).
func (c *Container) pushSubmodules(ctx context.Context, stdout, stderr io.Writer, containerRepoPath, hostGitRoot, tagRegexp string, quiet bool) error {
	g := &git.Checkout{Root: hostGitRoot, Logger: c.Logger}
	subs, err := g.ListSubmodules(ctx)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	moduleDirs, err := g.FindModuleDirs(ctx)
	if err != nil {
		return err
	}
	if len(moduleDirs) == 0 {
		// Submodules declared but not yet cloned on host — skip silently.
		return nil
	}
	if !quiet {
		_, _ = fmt.Fprintf(stdout, "- pushing %d submodule(s) ...\n", len(moduleDirs))
	}

	containerModulesBase := containerRepoPath + "/.git/modules"
	hostModulesBase := filepath.Join(hostGitRoot, ".git", "modules")

	for _, relPath := range moduleDirs {
		hostModuleDir := filepath.Join(hostModulesBase, relPath)
		// Use forward slashes: container is always Linux.
		containerModuleDir := containerModulesBase + "/" + filepath.ToSlash(relPath)

		// Initialize as bare so git push can transfer objects, then unset
		// core.bare (git submodule update sets core.worktree on the module
		// gitdir, which conflicts with core.bare=true). Also set
		// receive.denyCurrentBranch=ignore so that git push works even though
		// the repo is no longer bare after the unset.
		initCmd := "git init -q --bare " + shellQuote(containerModuleDir) +
			" && git -C " + shellQuote(containerModuleDir) + " config --unset core.bare" +
			" && git -C " + shellQuote(containerModuleDir) + " config receive.denyCurrentBranch ignore"
		if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, initCmd), stdout, stderr); err != nil {
			return fmt.Errorf("init submodule %s: %w", relPath, err)
		}
		// Push all local branch refs from the host bare module repo. Selected
		// tags are pushed separately below.
		// Use GIT_DIR env var instead of --git-dir because --git-dir
		// still reads core.worktree from the repo config and tries
		// to chdir there, which fails when the submodule worktree
		// was never checked out (init but not update, or deinited).
		// GIT_DIR fully decouples git from any worktree.
		containerURL := "user@" + c.Name + ":" + containerModuleDir
		if _, err := c.runGitDir(ctx, hostGitRoot, hostModuleDir, "push", "-q", "--no-verify", "--no-follow-tags", "--all", containerURL); err != nil {
			return fmt.Errorf("push submodule refs %s: %w", relPath, err)
		}
		if tagRegexp != "" {
			tags, err := c.runGitDir(ctx, hostGitRoot, hostModuleDir, "tag", "--list")
			if err != nil {
				return fmt.Errorf("list submodule tags %s: %w", relPath, err)
			}
			refspecs, err := tagRefspecs(tagRegexp, tags)
			if err != nil {
				return fmt.Errorf("select submodule tags %s: %w", relPath, err)
			}
			for start := 0; start < len(refspecs); {
				end := refspecBatchEnd(refspecs, start)
				args := make([]string, 0, 5+end-start)
				args = append(args, "push", "-q", "--no-verify", "--no-follow-tags", containerURL)
				args = append(args, refspecs[start:end]...)
				if _, err := c.runGitDir(ctx, hostGitRoot, hostModuleDir, args...); err != nil {
					return fmt.Errorf("push submodule tags %s: %w", relPath, err)
				}
				start = end
			}
		}
	}

	// Recursive function: at each nesting level, init submodules, override
	// URLs to local module-gitdir paths, update without network, then recurse.
	//
	// __md_sm_visit traverses $gd/modules/ to find bare repos at any depth.
	// Submodule names can contain slashes (e.g. "bundle/ctrlp.vim"), which git
	// stores as nested directories under modules/ with the full name as the
	// relative path. A one-level glob would match the intermediate "bundle/"
	// directory (not a bare repo) and miss the actual submodule. We detect bare
	// repos by the presence of HEAD + objects/ + refs/ and recurse into
	// non-bare directories to handle these intermediate path components.
	// Script driven by .gitmodules (canonical declaration), sourcing data
	// from .git/modules/ (pushed from host). Each failure mode prints a
	// prefixed message so the user can diagnose missing host-side
	// submodule init, clone conflicts, or partial checkouts.
	script := "cd " + shellQuote(containerRepoPath) + ` && __md_sm_fix() {
  local gd line name val names
  gd=$(git rev-parse --git-dir)
  [ -d "$gd/modules" ] || return 0

  # Collect submodules declared in .gitmodules whose data exists locally.
  names=()
  while IFS= read -r line; do
    name="${line#submodule.}"
    name="${name%.path}"
    if [ ! -d "$gd/modules/$name" ]; then
      echo "md: submodule '$name': not initialized on host (no .git/modules/$name)" >&2
      continue
    fi
    names+=("$name")
  done < <(git config --file .gitmodules --name-only --get-regexp '^submodule\..*\.path$')
  [ "${#names[@]}" -gt 0 ] || return 0

  # Phase 1: init and point URL to local module dir.
  for name in "${names[@]}"; do
    val=$(git config --file .gitmodules "submodule.$name.path")
    if ! git submodule init -q -- "$val"; then
      echo "md: submodule '$name': init failed" >&2
    fi
    git config "submodule.$name.url" "$gd/modules/$name" ||
      echo "md: submodule '$name': url override failed" >&2
  done

  # Phase 2: remove stale directories left by previous failed checkouts,
  # then let git submodule update clone + checkout from local data.
  for name in "${names[@]}"; do
    val=$(git config --file .gitmodules "submodule.$name.path")
    [ -f "$val/.git" ] && continue
    [ -d "$val" ] && rm -rf "$val"
  done

  # Phase 3: clone from local module dirs and checkout (instant, no network).
  git -c advice.detachedHead=false submodule update ||
    echo "md: some submodules failed to update (check messages above)" >&2

  # Phase 4: recurse into checked-out submodules.
  for name in "${names[@]}"; do
    val=$(git config --file .gitmodules "submodule.$name.path")
    if [ -d "$val" ]; then
      (cd "$val" && __md_sm_fix) ||
        echo "md: submodule '$name': nested update failed" >&2
    fi
    true
  done
}
export -f __md_sm_fix && __md_sm_fix`
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, script), stdout, stderr); err != nil {
		return fmt.Errorf("submodule update: %w", err)
	}
	return nil
}

// launchContainer starts the Docker container, queries mapped ports, writes
// SSH config, and sets up host-side git remotes. It does NOT wait for SSH.
// Port and creation-time results are stored directly on c (launchSSHPort,
// launchVNCPort, CreatedAt) so that connectContainer can complete startup.
func (c *Container) launchContainer(ctx context.Context, stdout, stderr io.Writer, opts *StartOpts, imageName string) error {
	if len(c.Repos) > 1000 {
		return fmt.Errorf("too many repositories: %d (max 1000)", len(c.Repos))
	}
	if opts.Sudo && c.Runtime.IsRootless() {
		return errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	p := Platform(opts.Platform).Resolve()
	if err := p.Validate(); err != nil {
		return err
	}
	platform := p.String()
	runArgs := []string{
		"run", "-d",
		"--platform", platform,
		"--name", c.Name,
		"--hostname", c.Name,
		"-p", "127.0.0.1::22",
		// Localtime: mount the host timezone file. Docker Desktop on Windows/macOS provides
		// a virtual /etc/localtime inside the VM, so the flag is universal. The image keeps
		// /etc/localtime a regular file; mounting over a symlink would follow it and clobber
		// /usr/share/zoneinfo/Etc/UTC, breaking TZ=UTC inside the container.
		"-v", "/etc/localtime:/etc/localtime:ro",
	}
	if opts.MaxCPUs > 0 {
		runArgs = append(runArgs, "--cpus", strconv.Itoa(opts.MaxCPUs))
	}

	if opts.Display {
		runArgs = append(runArgs, "-p", "127.0.0.1::5901", "-e", "MD_DISPLAY=1")
	}

	if kvmAvailable() {
		runArgs = append(runArgs, "--device=/dev/kvm")
	}
	// Sandbox capabilities.
	// - SYS_PTRACE: needed for strace/debuggers. Scoped to the container's
	//   PID namespace — cannot attach to host processes.
	// - seccomp=unconfined: disables the syscall allowlist so strace, bpf,
	//   and Chrome's sandbox work. Does NOT grant capabilities — the
	//   capability set still limits what the process can do.
	runArgs = append(runArgs,
		"--cap-add=SYS_PTRACE",
		"--security-opt", "seccomp=unconfined")
	// - apparmor=unconfined: disables AppArmor's mandatory-access-control
	//   profile so Chrome can create namespaces and sandboxed processes can
	//   access /proc. Docker-only; podman uses SELinux and passing this
	//   option can hang on kernel security filesystem access.
	if c.Runtime.Name() != "podman" {
		runArgs = append(runArgs, "--security-opt", "apparmor=unconfined")
	}

	rootlessPodman := c.Runtime.Name() == "podman" && c.Runtime.IsRootless()
	hostIDs := hostUserEnv(rootlessPodman)
	runArgs = append(runArgs, hostIDs...)

	// Rootless Podman maps the host user to the image's UID/GID 1000. This makes
	// bind-mounted configs writable without rewriting and recursively chowning
	// the large image home. --user 0:0 keeps start.sh running as root for
	// privileged setup. Rootless Docker is handled inside start.sh via
	// /proc/self/uid_map detection since Docker lacks --userns=keep-id.
	//
	// Trade-off: keep-id ownership does not round-trip through `podman
	// commit`, so Fork must re-chown snapshotted repos. See docs/ROOTLESS.md.
	if rootlessPodman {
		runArgs = append(runArgs, rootlessPodmanUserNSArg(), "--user", "0:0")
	}

	// NET_ADMIN and NET_RAW are always granted:
	// - tcpdump uses AF_PACKET sockets which require NET_RAW.
	// - Tailscale manipulates the network interface (route table changes)
	//   which requires NET_ADMIN.
	// Both are scoped to the container's network namespace.
	runArgs = append(runArgs,
		"--cap-add=NET_ADMIN", "--cap-add=NET_RAW")

	// Pass through the host TUN device when Tailscale or rootless Podman
	// (via -sudo) need to create network interfaces.
	if opts.Tailscale || opts.Sudo {
		runArgs = append(runArgs, "--device=/dev/net/tun:/dev/net/tun")
	}

	// Tailscale.
	//
	// Two approaches exist for providing /dev/net/tun to the container:
	//
	//   1. --device=/dev/net/tun:/dev/net/tun (chosen): passes the host's
	//      pre-existing TUN device into the container. This is the approach
	//      recommended by Tailscale's official Docker image and blog posts.
	//      Pros: no MKNOD capability needed (MKNOD allows creating arbitrary
	//      device nodes — a known container breakout vector per
	//      hacktricks/angelica.gitbook.io). Avoids cgroup v2 device allowlist
	//      issues with dynamically-created nodes (containerd/containerd#11078).
	//      Cons: requires the host kernel to have the tun module loaded and
	//      /dev/net/tun present before container start.
	//
	//   2. --cap-add=MKNOD + internal mknod (dropped): the container creates
	//      its own /dev/net/tun with mknod c 10 200. Pros: works even if the
	//      host lacks /dev/net/tun (uncommon on modern systems). Cons: MKNOD
	//      is a security liability; dynamically-created device nodes may be
	//      blocked by cgroup v2 DeviceAllow rules in newer containerd/runc
	//      versions. Tailscale themselves moved away from this pattern.
	//
	// Ref: https://tailscale.com/kb/1282/docker (official Docker image docs)
	// Ref: https://tailscale.com/blog/docker-tailscale-guide (blog post)
	// Ref: https://github.com/containerd/containerd/issues/11078 (cgroup v2
	//      breakage of internal mknod)
	if opts.Tailscale {
		runArgs = append(runArgs,
			"-e", "MD_TAILSCALE=1")
		if opts.TailscaleAuthKey != "" {
			runArgs = append(runArgs, "-e", "TAILSCALE_AUTHKEY="+opts.TailscaleAuthKey)
		}
		if c.tailscaleEphemeral {
			runArgs = append(runArgs, "-e", "MD_TAILSCALE_EPHEMERAL=1")
		}
		if opts.resetTailscale {
			runArgs = append(runArgs, "-e", "MD_TAILSCALE_RESET=1")
		}
	}

	// USB passthrough (Linux only; Docker Desktop on macOS/Windows runs in a
	// VM that cannot access host USB devices). Use a bind mount + cgroup rule
	// so raw USB devices plugged in after container start are visible. USB
	// serial adapters are separate character devices, so attach the adapters
	// present at container start as well.
	if opts.USB {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("--usb requires Linux; Docker Desktop on %s cannot pass through host USB devices", runtime.GOOS)
		}
		serialDevices, err := hostUSBSerialDevices()
		if err != nil {
			return err
		}
		runArgs = append(runArgs, usbRunArgs(serialDevices)...)
	}

	home := c.Home
	for _, m := range opts.Mounts {
		arg, err := m.dockerArg(home)
		if err != nil {
			return err
		}
		runArgs = append(runArgs, "-v", arg)
	}

	// Set md metadata labels.
	if opts.Sudo {
		sudoPassword, err := generatePassword()
		if err != nil {
			return fmt.Errorf("generating sudo password: %w", err)
		}
		c.sudoPassword = sudoPassword
		// SYS_ADMIN: allows start.sh to remount /proc and unmask Docker's
		// /proc tmpfs mounts, both required for nested user namespaces.
		// /dev/fuse:  required by fuse-overlayfs, the default rootless
		// Podman storage driver.
		// See: https://www.redhat.com/sysadmin/podman-inside-container
		runArgs = append(runArgs,
			"--label", "md.sudo=1",
			"--label", "md.sudo-password="+sudoPassword,
			"-e", "MD_SUDO_PASSWORD="+sudoPassword,
			"--cap-add=SYS_ADMIN",
			"--device=/dev/fuse")
	}
	if reposJSON, err := json.Marshal(c.Repos); err == nil {
		// Base64-encode so commas in JSON don't corrupt the comma-separated
		// label parsing in unmarshalContainer.
		runArgs = append(runArgs, "--label", "md.repos="+base64.StdEncoding.EncodeToString(reposJSON))
	}
	if opts.Display {
		runArgs = append(runArgs, "--label", "md.display=1")
	}
	if opts.Tailscale {
		runArgs = append(runArgs, "--label", "md.tailscale=1")
		if c.tailscaleEphemeral {
			runArgs = append(runArgs, "--label", "md.tailscale_ephemeral=1")
		}
	}
	if opts.USB {
		runArgs = append(runArgs, "--label", "md.usb=1")
	}
	for _, l := range opts.Labels {
		runArgs = append(runArgs, "--label", l)
	}
	runArgs = append(runArgs, opts.ExtraRunArgs...)
	runArgs = append(runArgs, imageName)

	if opts.Quiet {
		if _, err := c.Runtime.Run(ctx, "", runArgs...); err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "- Starting container %s ... ", c.Name)
		if err := c.Runtime.RunOut(ctx, "", stdout, stderr, runArgs...); err != nil {
			_, _ = fmt.Fprintln(stdout)
			return fmt.Errorf("starting container: %w", err)
		}
	}
	c.State = "running"

	// Get creation time and port mappings in one inspect call.
	if err := c.refreshRuntimeFields(ctx); err != nil {
		return fmt.Errorf("inspecting started container: %w", err)
	}
	port := c.SSHPort
	if port == 0 {
		return fmt.Errorf("container %s has no SSH port mapping", c.Name)
	}
	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Found ssh port %d\n", port)
	}
	if opts.Display && c.VNCPort != 0 && !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Found VNC port %d (display :1)\n", c.VNCPort)
	}

	// Write SSH config.
	sshConfigDir := filepath.Join(home, ".ssh", "config.d")
	if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
		return err
	}
	c.sshConfigPath = filepath.Join(sshConfigDir, c.Name+".conf")
	knownHostsPath := filepath.Join(sshConfigDir, c.Name+".known_hosts")
	hostPubKey, err := os.ReadFile(c.HostKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("reading host public key: %w", err)
	}
	if err := writeSSHConfig(sshConfigDir, c.Name, port, c.UserKeyPath, knownHostsPath, c.ControlMaster); err != nil {
		return err
	}
	if err := writeKnownHosts(knownHostsPath, port, strings.TrimSpace(string(hostPubKey))); err != nil {
		return err
	}

	// Set up git remotes for all repos before waiting for SSH, so they are
	// ready to push as soon as the connection is established.
	if len(c.Repos) > 0 {
		for i := range c.Repos {
			r := &c.Repos[i]
			rPath := r.ContainerPath
			_, _ = c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "rm", c.Name})
			if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "remote", "add", c.Name, "user@" + c.Name + ":" + rPath}, stdout, stderr); err != nil {
				return fmt.Errorf("adding git remote for %s: %w", rPath, err)
			}
		}
	}
	return nil
}

// hostUSBSerialDevices returns the USB serial device nodes currently attached
// to the host. They must be added individually because they are separate from
// the raw USB bus device hierarchy.
func hostUSBSerialDevices() ([]string, error) {
	var paths []string
	for _, pattern := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("finding USB serial devices with %q: %w", pattern, err)
		}
		for _, path := range matches {
			info, err := os.Stat(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("checking USB serial device %q: %w", path, err)
			}
			if info.Mode()&os.ModeCharDevice != 0 {
				paths = append(paths, path)
			}
		}
	}
	return paths, nil
}

// usbRunArgs returns runtime arguments that expose raw USB and the specified
// USB serial device nodes to a container.
func usbRunArgs(serialDevices []string) []string {
	args := make([]string, 0, 3+len(serialDevices))
	args = append(args,
		"-v", "/dev/bus/usb:/dev/bus/usb",
		"--device-cgroup-rule=c 189:* rwm",
	)
	for _, path := range serialDevices {
		args = append(args, "--device="+path)
	}
	return args
}

func rootlessPodmanUserNSArg() string {
	return fmt.Sprintf("--userns=keep-id:uid=%d,gid=%d", containerUserUID, containerUserGID)
}

func hostUserEnv(rootlessPodman bool) []string {
	uid := os.Getuid()
	gid := os.Getgid()
	if uid <= 0 || gid <= 0 {
		return nil
	}
	if rootlessPodman {
		uid = containerUserUID
		gid = containerUserGID
	}
	return []string{"-e", "MD_HOST_UID=" + strconv.Itoa(uid), "-e", "MD_HOST_GID=" + strconv.Itoa(gid)}
}

// waitForTCP polls until a TCP connection to addr succeeds or the deadline is
// exceeded.
func waitForTCP(ctx context.Context, addr string, deadline time.Time) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for TCP %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tryReadTailscaleAuthURL reads the Tailscale auth URL from the container.
// A single docker exec runs a polling loop: jq validates the JSON file and
// prints it compactly; if invalid or empty, sleep and retry. Go parses the
// result with tailscaleUpStatus for validation.
func (c *Container) tryReadTailscaleAuthURL(ctx context.Context, stdout io.Writer) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	script := `while true; do
  if jq -ce '.' /run/md/tailscale_auth_url.json 2>/dev/null; then exit 0; fi
  sleep 0.1
done`
	out, err := c.Runtime.Run(readCtx, "", "exec", c.Name, "sh", "-c", script)
	if err != nil || out == "" {
		return "", errors.New("timed out waiting for tailscale up --json output")
	}

	var status tailscaleUpStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return "", fmt.Errorf("parsing tailscale up --json output: %w (%q)", err, out)
	}
	if status.AuthURL == "" {
		return "", errors.New("tailscale up --json had no AuthURL field")
	}
	_, _ = fmt.Fprintf(stdout, "- Tailscale auth URL: %s\n", status.AuthURL)
	return status.AuthURL, nil
}

// provisionContainer waits for SSH, pushes repos and submodules, sends .env,
// and waits for Tailscale auth. Must be called after launchContainer.
//
// Mapped branches and default refs are pushed together to reduce latency.
func (c *Container) provisionContainer(ctx context.Context, stdout, stderr io.Writer, opts *StartOpts) (*StartResult, error) {
	result := &StartResult{}

	// Try to read the Tailscale auth URL via docker exec before SSH is up,
	// so the user can authenticate even if SSHD is slow to start.
	if opts.Tailscale && opts.TailscaleAuthKey == "" {
		authURL, err := c.tryReadTailscaleAuthURL(ctx, stdout)
		if err != nil {
			return nil, fmt.Errorf("reading Tailscale auth URL: %w", err)
		}
		result.TailscaleAuthURL = authURL
	}

	// Phase 1: wait for SSH to accept connections.
	addr := fmt.Sprintf("localhost:%d", c.SSHPort)
	if err := waitForTCP(ctx, addr, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return nil, err
	}
	if err := c.waitForSSH(ctx, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return nil, err
	}
	if err := c.configureGlobalGitIdentity(ctx, stdout, stderr); err != nil {
		return nil, err
	}

	// Phase 2: push all repos into the container in parallel, including
	// submodules. Each repo pushes to a distinct path (~/src/<name>).
	//
	// Host-side branch tips that do not match a cached remote-tracking ref are
	// pushed into refs/md/incoming/<name> as checkout seeds. Each working branch
	// tracks the same upstream as its corresponding host branch.
	if len(c.Repos) > 0 {
		if !opts.Quiet {
			_, _ = fmt.Fprintln(stdout, "- git clone into container ...")
		}
		eg, egCtx := errgroup.WithContext(ctx)
		for repoIdx := range c.Repos {
			eg.Go(func() error {
				r := &c.Repos[repoIdx]
				mp := shellQuote(r.ContainerPath)

				if err := c.runCmdOut(egCtx, "", c.SSHCommand(nil, "git init -q "+mp), stdout, stderr); err != nil {
					return fmt.Errorf("init repo %s in container: %w", r.ContainerPath, err)
				}
				if err := r.resolveDefaults(egCtx, c.Logger); err != nil {
					return fmt.Errorf("resolve defaults for %s: %w", r.ContainerPath, err)
				}
				bases, err := c.pushMappedBranchRefs(egCtx, stdout, stderr, r)
				if err != nil {
					return fmt.Errorf("push repo %s: %w", r.ContainerPath, err)
				}
				remoteURL, _ := c.runCmd(egCtx, r.GitRoot, []string{"git", "remote", "get-url", r.DefaultRemote})
				httpsURL := convertGitURLToHTTPS(remoteURL)
				if !opts.Quiet && httpsURL != "" {
					_, _ = fmt.Fprintf(stdout, "- Set %s %s to %s\n", r.ContainerPath, r.DefaultRemote, httpsURL)
				}
				if err := c.configureContainerRemotes(egCtx, stdout, stderr, repoIdx, true, containerBranchSetupCommands(bases)...); err != nil {
					return err
				}
				if err := c.recordHostBranchSyncPoints(egCtx, r); err != nil {
					return err
				}

				if err := c.pushSubmodules(egCtx, stdout, stderr, r.ContainerPath, r.GitRoot, r.TagRegexp, opts.Quiet); err != nil {
					return fmt.Errorf("push submodules for %s: %w", r.ContainerPath, err)
				}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
	}

	// Phase 3: send .env into the container, combining per-repo .env files
	// and extra environment updates from opts.
	if err := c.sendEnv(ctx, stdout, opts); err != nil {
		return nil, err
	}

	return result, nil
}

func renderExtraEnv(entries []string) ([]byte, error) {
	var result []byte
	for _, entry := range entries {
		line, err := renderExtraEnvEntry(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, line...)
		result = append(result, '\n')
	}
	return result, nil
}

func renderExtraEnvEntry(entry string) (string, error) {
	name, value, ok := strings.Cut(entry, "=")
	if !ok || !validExtraEnvName(name) {
		return "", fmt.Errorf("invalid extra env %q: use NAME=value or NAME= to unset", entry)
	}
	if value == "" {
		return "unset " + name, nil
	}
	return name + "=" + shellSingleQuote(value), nil
}

func validExtraEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// sendEnv combines per-repo .env files and extra environment updates from
// opts, then copies them into the container at /home/user/.env.
func (c *Container) sendEnv(ctx context.Context, stdout io.Writer, opts *StartOpts) error {
	var envContent []byte
	for i := range c.Repos {
		data, err := os.ReadFile(filepath.Join(c.Repos[i].GitRoot, ".env"))
		if err != nil {
			continue
		}
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		envContent = append(envContent, data...)
	}
	if len(opts.ExtraEnv) > 0 {
		extraEnv, err := renderExtraEnv(opts.ExtraEnv)
		if err != nil {
			return err
		}
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		envContent = append(envContent, extraEnv...)
		if !opts.Quiet {
			_, _ = fmt.Fprintln(stdout, "- injecting extra env vars into container ...")
		}
	}
	if len(envContent) == 0 {
		// No repo .env and no extra env means there is nothing to copy into the
		// container.
		return nil
	}
	if !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- sending .env into container ...")
	}
	sshEnvArgs := c.SSHCommand(nil, "cat > /home/user/.env")
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "cmd", sshEnvArgs)
	cmd := exec.CommandContext(ctx, sshEnvArgs[0], sshEnvArgs[1:]...) //nolint:gosec // args are from trusted SSH config
	cmd.Env = c.commandEnv()
	cmd.Stdin = bytes.NewReader(envContent)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("copying .env: %w\n%s", err, out)
	}
	return nil
}

// forkAttempt records external state created by one Fork call. On failure it
// rolls host branches back only if they still contain the exact ref and
// configuration this call wrote, then removes the partial container and
// snapshot tag.
type forkAttempt struct {
	source          *Container
	fork            *Container
	snapshotImage   string
	hostBranches    []forkHostBranchChange
	snapshotCreated bool
}

func (a *forkAttempt) rollback(ctx context.Context) error {
	var retErr error
	for i := len(a.hostBranches) - 1; i >= 0; i-- {
		retErr = errors.Join(retErr, a.hostBranches[i].rollback(ctx, a.source.Logger))
	}
	if a.fork != nil && a.fork.State != "" {
		retErr = errors.Join(retErr, a.fork.Purge(ctx, io.Discard, io.Discard))
	}
	if a.snapshotCreated {
		retErr = errors.Join(retErr, a.source.untagImage(ctx, a.snapshotImage))
	}
	return retErr
}

// convertGitURLToHTTPS converts a git URL to HTTPS format.
//
// Supports git@host:path, ssh://git@host/path, git://host/path, and HTTPS.
// URL user information is removed so host credentials are not copied into the
// container.
func convertGitURLToHTTPS(rawURL string) string {
	converted := strings.TrimSpace(rawURL)
	// Matches git@host:user/repo.git
	if m := reGitAt.FindStringSubmatch(converted); m != nil {
		converted = fmt.Sprintf("https://%s/%s", m[1], m[2])
	} else if m := reSSHGit.FindStringSubmatch(converted); m != nil {
		// Matches ssh://git@host/user/repo.git
		converted = fmt.Sprintf("https://%s/%s", m[1], m[2])
	} else if m := reGitProto.FindStringSubmatch(converted); m != nil {
		// Matches git://host/user/repo.git
		converted = fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	parsed, err := url.Parse(converted)
	if err != nil {
		return ""
	}
	parsed.User = nil
	return parsed.String()
}

// generatePassword creates a random 20-character alphanumeric password
// suitable for the container sudo account.
func generatePassword() (string, error) {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	const pwdLen = 20
	// ceil(pwdLen * log2(62) / 8) = 15 bytes of entropy.
	var raw [15]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating sudo password: %w", err)
	}
	// Convert random bytes to base-62 — perfectly unbiased, no rejection.
	n := new(big.Int).SetBytes(raw[:])
	var pwd [pwdLen]byte
	var rem big.Int
	base := big.NewInt(62)
	for i := len(pwd) - 1; i >= 0; i-- {
		n.DivMod(n, base, &rem)
		pwd[i] = chars[rem.Int64()]
	}
	return string(pwd[:]), nil
}

// shellQuote returns a shell-escaped version of s, safe for embedding in a
// single-quoted shell string.  Equivalent to Python's shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// If the string contains only safe characters, return it as-is.
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '@' && c != '%' && c != '+' && c != '=' && c != ':' && c != ',' && c != '.' &&
			c != '/' && c != '-' && c != '_' {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

func shellQuoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// psName handles Docker's string format and Podman's array format for the
// Names field in `docker ps --format '{{json .}}'`.
type psName string

func (n *psName) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '[' {
		var names []string
		if err := json.Unmarshal(data, &names); err != nil {
			return err
		}
		if len(names) > 0 {
			*n = psName(names[0])
		}
		return nil
	}
	return json.Unmarshal(data, (*string)(n))
}

type podmanPortMapping struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Proto         string `json:"proto"`
}

// psPorts handles Docker's string format and Podman's array format for the
// Ports field in `docker ps --format '{{json .}}'`.
type psPorts string

func (p *psPorts) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '[' {
		var mappings []podmanPortMapping
		if err := json.Unmarshal(data, &mappings); err != nil {
			return err
		}
		var parts []string
		for _, m := range mappings {
			host := m.HostIP
			if host == "" {
				host = "0.0.0.0"
			}
			parts = append(parts, fmt.Sprintf("%s:%d->%d/%s", host, m.HostPort, m.ContainerPort, m.Proto))
		}
		*p = psPorts(strings.Join(parts, ", "))
		return nil
	}
	return json.Unmarshal(data, (*string)(p))
}

// psLabels handles Docker's comma-separated string format and Podman's JSON
// object format for the Labels field in `docker ps --format '{{json .}}'`.
type psLabels map[string]string

func (l *psLabels) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	*l = make(map[string]string)
	if data[0] == '{' {
		return json.Unmarshal(data, (*map[string]string)(l))
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for kv := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		(*l)[k] = v
	}
	return nil
}

// containerJSON is the raw Docker ps JSON structure.
type containerJSON struct {
	Names     psName   `json:"Names"`
	State     string   `json:"State"`
	CreatedAt string   `json:"CreatedAt"`
	Labels    psLabels `json:"Labels"`
	Ports     psPorts  `json:"Ports"`
}

// InspectInfo describes observed Docker/Podman runtime configuration for a container.
type InspectInfo struct {
	Runtime      string
	ID           string
	Name         string
	State        string
	ImageRef     string
	ImageID      string
	Platform     string
	OS           string
	Architecture string
	CPULimit     int
	Mounts       []Mount
	Caches       []CacheMount
}

func inspectInfoFromRuntime(info *containers.InspectInfo) *InspectInfo {
	mounts := make([]Mount, len(info.Mounts))
	for i, m := range info.Mounts {
		mounts[i] = Mount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	return &InspectInfo{
		Runtime:      info.Runtime,
		ID:           info.ID,
		Name:         info.Name,
		State:        info.State,
		ImageRef:     info.ImageRef,
		ImageID:      info.ImageID,
		Platform:     info.Platform,
		OS:           info.OS,
		Architecture: info.Architecture,
		CPULimit:     info.CPULimit,
		Mounts:       mounts,
		Caches:       cacheSpecFromLabel(info.Labels["md.cache_spec"]),
	}
}

// containerInspectJSON is the subset of `docker inspect` output we parse.
type containerInspectJSON struct {
	ID              string             `json:"Id"`
	Name            string             `json:"Name"`
	Image           string             `json:"Image"`
	Platform        string             `json:"Platform"`
	Architecture    string             `json:"Architecture"`
	OS              string             `json:"Os"`
	State           inspectStateJSON   `json:"State"`
	Created         string             `json:"Created"`
	Config          inspectConfigJSON  `json:"Config"`
	HostConfig      inspectHostJSON    `json:"HostConfig"`
	NetworkSettings inspectNetworkJSON `json:"NetworkSettings"`
	Mounts          []inspectMountJSON `json:"Mounts"`
}

type inspectConfigJSON struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type inspectHostJSON struct {
	NanoCpus  int64 `json:"NanoCpus"`
	CPUQuota  int64 `json:"CpuQuota"`
	CPUPeriod int64 `json:"CpuPeriod"`
}

type inspectNetworkJSON struct {
	Ports map[string][]inspectPortBindingJSON `json:"Ports"`
}

type inspectPortBindingJSON struct {
	HostPort string `json:"HostPort"`
}

type inspectMountJSON struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          *bool  `json:"RW"`
}

type inspectStateJSON struct {
	Status string `json:"Status"`
}

// parseCreatedAt parses a container creation timestamp. Docker uses
// "2006-01-02 15:04:05 -0700 MST"; Podman uses ISO 8601 (RFC 3339).
func parseCreatedAt(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05 -0700 MST",           // Docker
		time.RFC3339Nano,                          // Podman
		time.RFC3339,                              // Podman (no fractional seconds)
		"2006-01-02 15:04:05.999999999 -0700 MST", // Docker with nanoseconds
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse CreatedAt %q", s)
}

// unmarshalContainer parses docker/podman ps JSON output, converting the
// CreatedAt timestamp string into a time.Time and extracting md.* labels.
// The returned Container has a nil Client; callers must set it.
func unmarshalContainer(ctx context.Context, client *Client, data []byte) (Container, error) {
	var raw containerJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Container{}, err
	}
	ct := Container{
		Logger: client.Logger.With(slog.String("cntr", string(raw.Names))),
		Name:   string(raw.Names),
		State:  raw.State,
		Labels: maps.Clone(map[string]string(raw.Labels)),
	}
	if raw.CreatedAt != "" {
		t, err := parseCreatedAt(raw.CreatedAt)
		if err != nil {
			return Container{}, err
		}
		ct.CreatedAt = t
	}
	for k, v := range raw.Labels {
		switch k {
		case "md.repos":
			if data, err := base64.StdEncoding.DecodeString(v); err == nil {
				if err := json.Unmarshal(data, &ct.Repos); err != nil {
					ct.Logger.Log(ctx, slog.LevelWarn, "failed to unmarshal repos label", "err", err)
				}
				ct.migrateRepoRemotes(ctx)
				for i := range ct.Repos {
					if err := ct.Repos[i].Validate(); err != nil {
						return Container{}, fmt.Errorf("unmarshal repos[%d]: %w", i, err)
					}
				}
			}
		case "md.display":
			ct.Display = v == "1"
		case "md.tailscale":
			ct.Tailscale = v == "1"
		case "md.usb":
			ct.USB = v == "1"
		case "md.sudo":
			ct.Sudo = v == "1"
		}
	}
	// Parse port mappings: "0.0.0.0:32768->22/tcp, 0.0.0.0:32769->5901/tcp"
	for mapping := range strings.SplitSeq(string(raw.Ports), ",") {
		mapping = strings.TrimSpace(mapping)
		if mapping == "" {
			continue
		}
		// Cut on "->" to get host:port and containerPort/proto.
		hostPart, containerPart, ok := strings.Cut(mapping, "->")
		if !ok {
			continue
		}
		containerPortStr, _, _ := strings.Cut(containerPart, "/")
		hostPortStr := hostPart
		if idx := strings.LastIndex(hostPart, ":"); idx >= 0 {
			hostPortStr = hostPart[idx+1:]
		}
		hostPort, err := strconv.ParseInt(hostPortStr, 10, 32)
		if err != nil {
			continue
		}
		containerPort, err := strconv.ParseInt(containerPortStr, 10, 32)
		if err != nil {
			continue
		}
		switch int32(containerPort) {
		case 22:
			ct.SSHPort = int32(hostPort)
		case 5901:
			ct.VNCPort = int32(hostPort)
		}
	}
	return ct, nil
}

func parseInspectInfo(runtimeName, requestedName string, data []byte) (*InspectInfo, error) {
	raw, err := inspectDocument(data)
	if err != nil {
		return nil, err
	}
	mounts := make([]Mount, 0, len(raw.Mounts))
	for _, m := range raw.Mounts {
		if m.Source == "" && m.Destination == "" {
			continue
		}
		readOnly := false
		if m.RW != nil {
			readOnly = !*m.RW
		}
		mounts = append(mounts, Mount{HostPath: m.Source, ContainerPath: m.Destination, ReadOnly: readOnly})
	}
	name := strings.TrimPrefix(raw.Name, "/")
	if name == "" {
		name = requestedName
	}
	osName, architecture := inspectOSArch(&raw)
	return &InspectInfo{
		Runtime:      runtimeName,
		ID:           raw.ID,
		Name:         name,
		State:        raw.State.Status,
		ImageRef:     raw.Config.Image,
		ImageID:      raw.Image,
		Platform:     inspectPlatform(&raw),
		OS:           osName,
		Architecture: architecture,
		CPULimit:     inspectCPULimit(raw.HostConfig),
		Mounts:       mounts,
		Caches:       cacheSpecFromLabel(raw.Config.Labels["md.cache_spec"]),
	}, nil
}

func inspectDocument(data []byte) (containerInspectJSON, error) {
	var raws []containerInspectJSON
	if err := json.Unmarshal(data, &raws); err != nil {
		return containerInspectJSON{}, err
	}
	if len(raws) != 1 {
		return containerInspectJSON{}, fmt.Errorf("inspect returned %d results, expected 1", len(raws))
	}
	return raws[0], nil
}

func inspectPlatform(raw *containerInspectJSON) string {
	if raw.Platform != "" {
		return raw.Platform
	}
	osName, architecture := inspectOSArch(raw)
	if osName != "" && architecture != "" {
		return osName + "/" + architecture
	}
	return ""
}

func inspectOSArch(raw *containerInspectJSON) (osName, architecture string) {
	if osName, architecture, ok := splitOSArch(raw.Platform, ""); ok {
		return osName, architecture
	}
	osName = cleanInspectValue(raw.OS)
	if osName == "" {
		osName = cleanInspectValue(raw.Platform)
	}
	architecture = cleanInspectValue(raw.Architecture)
	return osName, architecture
}

func splitOSArch(platform, fallbackOS string) (osName, architecture string, ok bool) {
	osName, architecture, ok = strings.Cut(platform, "/")
	if !ok {
		return "", "", false
	}
	osName = cleanInspectValue(osName)
	if osName == "" {
		osName = fallbackOS
	}
	architecture = cleanInspectValue(architecture)
	if osName == "" || architecture == "" {
		return "", "", false
	}
	return osName, architecture, true
}

func cleanInspectValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "<no value>" {
		return ""
	}
	return v
}

func inspectCPULimit(c inspectHostJSON) int {
	if c.NanoCpus > 0 {
		return int((c.NanoCpus + 1_000_000_000 - 1) / 1_000_000_000)
	}
	if c.CPUQuota > 0 && c.CPUPeriod > 0 {
		return int((c.CPUQuota + c.CPUPeriod - 1) / c.CPUPeriod)
	}
	return 0
}

func cacheSpecFromLabel(label string) []CacheMount {
	if label == "" || label == "<no value>" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(label)
	if err != nil {
		return nil
	}
	var labelMounts []cacheSpecLabelMount
	if err := json.Unmarshal(data, &labelMounts); err != nil {
		return nil
	}
	caches := make([]CacheMount, len(labelMounts))
	for i, m := range labelMounts {
		caches[i] = CacheMount(m)
	}
	return caches
}

// tailscaleStatus is the subset of `tailscale status --json` we care about.
type tailscaleStatus struct {
	Self struct {
		ID      string `json:"ID"`
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}
