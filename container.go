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
	"log/slog"
	"maps"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

	hostRemoteSetupCommand = "git config --replace-all remote.host.url . && (git config --unset-all remote.host.pushurl >/dev/null 2>&1 || true) && git config --replace-all remote.host.fetch '+refs/remotes/host/*:refs/remotes/host/*'"
	// gitBaseRefCommand resolves base_ref and diff_base_ref for container diffs.
	// It resolves the current upstream, falls back to the upstream of the branch
	// recorded by an in-progress rebase, supports the legacy base branch, then
	// uses the merge-base with HEAD for diff_base_ref when possible.
	gitBaseRefCommand = `if ! base_ref=$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null); then
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
	echo 'no upstream branch configured' >&2
	exit 128
fi
diff_base_ref=$base_ref
if merge_base=$(git merge-base HEAD "$base_ref" 2>/dev/null) && [ -n "$merge_base" ]; then
	diff_base_ref=$merge_base
fi`
)

var forkSnapshotEnvKeys = [...]string{
	"MD_DISPLAY",
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
	// used for container naming). Branches[1:] are also available as
	// local branches in the container, tracked from the fake "host" remote.
	Branches []string `json:"branches"`
	// MountedPath is the absolute destination path inside the
	// container, e.g. "/home/user/src/github/caic". When empty,
	// populateMountPath fills it from filepath.Base(GitRoot).
	// When two repos share the same basename, resolveMountPaths
	// disambiguates using relative paths from /home/user/src.
	// Callers may set it explicitly to override.
	MountedPath string `json:"mounted_path,omitempty"`
	// DefaultRemote is the host's default git remote.
	DefaultRemote string `json:"default_remote,omitempty"`
	// DefaultBranch is the default branch for DefaultRemote.
	DefaultBranch string `json:"default_branch,omitempty"`
}

// UnmarshalJSON decodes Repo, including legacy labels that used "branch".
func (r *Repo) UnmarshalJSON(data []byte) error {
	type repoJSON Repo
	var raw repoJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = Repo(raw)
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
	r.populateMountPath()
	r.MountedPath = ResolveContainerPath(r.MountedPath)
	if r.GitRoot == "" {
		return errors.New("Repo.GitRoot is empty")
	}
	if len(r.Branches) == 0 {
		return errors.New("Repo.Branches is empty")
	}
	seenBranches := make(map[string]struct{}, len(r.Branches))
	for _, branch := range r.Branches {
		if branch == "" {
			return errors.New("Repo.Branches contains an empty branch")
		}
		if strings.ContainsFunc(branch, unicode.IsSpace) {
			return fmt.Errorf("Repo.Branches contains branch %q with whitespace", branch)
		}
		if _, ok := seenBranches[branch]; ok {
			return fmt.Errorf("Repo.Branches contains duplicate branch %q", branch)
		}
		seenBranches[branch] = struct{}{}
	}
	if r.MountedPath == "" {
		return errors.New("Repo.MountedPath could not be determined from GitRoot")
	}
	if !path.IsAbs(r.MountedPath) {
		return fmt.Errorf("Repo.MountedPath must be an absolute POSIX path, got %q", r.MountedPath)
	}
	return nil
}

// resolveDefaults populates DefaultRemote and DefaultBranch if not already set.
func (r *Repo) resolveDefaults(ctx context.Context, logger Logger) error {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	if r.DefaultRemote == "" {
		remote, err := g.DefaultRemote(ctx)
		if err != nil {
			return err
		}
		r.DefaultRemote = remote
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

// populateMountPath sets MountedPath from GitRoot if not already set.
func (r *Repo) populateMountPath() {
	if r.MountedPath == "" {
		r.MountedPath = "/home/user/src/" + strings.TrimSuffix(filepath.Base(r.GitRoot), ".git")
	}
}

func (r *Repo) containerSyncRefspecs(ctx context.Context, logger Logger) ([]string, error) {
	// These refspecs refresh the container's copy of the default branch and the
	// tracked branches. Callers append them to the push that already transfers
	// the mapped branches.
	if err := r.resolveDefaults(ctx, logger); err != nil {
		return nil, fmt.Errorf("sync default branch: %w", err)
	}
	refspecs := []string{}
	src, ok, err := defaultBranchPushSource(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("sync default branch %q: %w", r.DefaultBranch, err)
	}
	if ok {
		refspecs = append(refspecs, src+":"+remoteTrackingRef(r.DefaultRemote, r.DefaultBranch))
	}
	// Containers can outlive branches recorded at launch. If a branch was
	// deleted locally and remotely, leave the existing container refs in place.
	for _, b := range r.Branches {
		branch, src, ok, err := r.trackedBranchPushSource(ctx, logger, b)
		if err != nil {
			return nil, fmt.Errorf("sync tracked branch for %q: %w", b, err)
		}
		if ok {
			refspecs = appendUniqueRefspecs(refspecs, src+":"+remoteTrackingRef(r.DefaultRemote, branch))
		}
	}
	return refspecs, nil
}

// resolveContainerBranchBase chooses the ref that should seed branch inside the
// container and the destination ref to push into the container remote.
//
// It prefers the branch's configured upstream when it matches the local branch,
// then the default remote's same-named tracking branch, and finally the local
// branch via refs/remotes/host when the local branch contains unpushed commits.
func (r *Repo) resolveContainerBranchBase(ctx context.Context, logger Logger, branch string) (containerBranchBase, error) {
	g := &git.Checkout{Root: r.GitRoot, Logger: logger}
	localRef := "refs/heads/" + branch
	localCommit, err := g.RevParse(ctx, localRef)
	if err != nil {
		return containerBranchBase{}, err
	}
	remote, upstreamBranch, ok, err := r.branchUpstream(ctx, logger, branch)
	if err != nil {
		return containerBranchBase{}, err
	}
	if ok && remote == r.DefaultRemote {
		remoteRef := remoteTrackingRef(remote, upstreamBranch)
		if remoteCommit, err := g.RevParse(ctx, remoteRef); err == nil && remoteCommit == localCommit {
			return containerBranchBase{source: remoteRef, ref: remote + "/" + upstreamBranch, destination: remoteRef}, nil
		}
	}
	remoteRef := remoteTrackingRef(r.DefaultRemote, branch)
	if remoteCommit, err := g.RevParse(ctx, remoteRef); err == nil && remoteCommit == localCommit {
		return containerBranchBase{source: remoteRef, ref: r.DefaultRemote + "/" + branch, destination: remoteRef}, nil
	}
	return containerBranchBase{source: localRef, ref: "host/" + branch, useHost: true, destination: "refs/remotes/host/" + branch}, nil
}

// trackedBranchPushSource returns the default-remote tracking ref for branch's
// configured upstream.
//
// ok is false when branch has no upstream, its upstream is on another remote,
// or the upstream tracking ref is unavailable locally.
func (r *Repo) trackedBranchPushSource(ctx context.Context, logger Logger, branch string) (upstreamBranch, src string, ok bool, err error) {
	remote, upstreamBranch, ok, err := r.branchUpstream(ctx, logger, branch)
	if err != nil || !ok || remote != r.DefaultRemote {
		return "", "", false, err
	}
	src = remoteTrackingRef(remote, upstreamBranch)
	if _, err := (&git.Checkout{Root: r.GitRoot, Logger: logger}).RevParse(ctx, src); err != nil {
		return "", "", false, err
	}
	return upstreamBranch, src, true, nil
}

// branchUpstream returns branch's configured upstream remote and branch name.
//
// ok is false when branch has no upstream. An error is returned when git reports
// malformed upstream metadata.
func (r *Repo) branchUpstream(ctx context.Context, logger Logger, branch string) (remote, upstreamBranch string, ok bool, err error) {
	out, err := (&git.Checkout{Root: r.GitRoot, Logger: logger}).RunGit(ctx, "for-each-ref", "--format=%(upstream:remotename)%00%(upstream:remoteref)", "refs/heads/"+branch)
	if err != nil {
		return "", "", false, err
	}
	if out == "" {
		return "", "", false, nil
	}
	remote, remoteRef, ok := strings.Cut(out, "\x00")
	if ok && remote == "" && remoteRef == "" {
		return "", "", false, nil
	}
	if !ok || remote == "" || remoteRef == "" {
		return "", "", false, fmt.Errorf("invalid upstream for branch %q: %q", branch, out)
	}
	upstreamBranch, ok = strings.CutPrefix(remoteRef, "refs/heads/")
	if !ok || upstreamBranch == "" {
		return "", "", false, fmt.Errorf("invalid upstream ref for branch %q: %q", branch, remoteRef)
	}
	return remote, upstreamBranch, true, nil
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

// ProcessInfo describes a single process running inside a container.
type ProcessInfo struct {
	PID     int
	PPID    int
	User    string
	State   string
	CPU     float64
	Mem     float64
	Time    string
	Command string
}

// ForkOpts configures a container fork operation.
type ForkOpts struct {
	// ExtraRepos are additional repos to map into the fork beyond the
	// source container's repos. Branches are the source branches to push from
	// the host; if empty, defaults to the repo's default upstream branch.
	// Fork generates a unique destination for the primary branch only.
	ExtraRepos []Repo
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

// Container holds state for a single container instance.
//
// Fields marked with a label are persisted as Docker container labels
// and restored by [unmarshalContainer] when listing containers.
type Container struct {
	*Client

	// Repos are the git repositories in this container. Repos[0] is the
	// primary; the rest are pushed alongside it. Each repo's MountedPath
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

// Processes returns the running processes inside the container.
func (c *Container) Processes(ctx context.Context) ([]ProcessInfo, error) {
	cmd := "ps -eo pid,ppid,user,stat,%cpu,%mem,time,args --no-headers"
	sshArgs := c.SSHCommand(nil, cmd)
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "container", c.Name, "cmd", sshArgs)
	ec := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // SSH target is an md container name; command is a constant literal.
	ec.Env = c.commandEnv()
	out, err := ec.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ps in container %s: %w (output: %s)", c.Name, err, string(out))
	}
	return parsePSOutput(string(out))
}

// Signal sends sig to pid inside the container.
func (c *Container) Signal(ctx context.Context, pid int, sig string) error {
	if pid <= 0 {
		return fmt.Errorf("pid must be positive, got %d", pid)
	}
	cmd := fmt.Sprintf("kill -s %s %d", shellQuote(sig), pid)
	sshArgs := c.SSHCommand(nil, cmd)
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "container", c.Name, "cmd", sshArgs)
	ec := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // SSH target is an md container name; pid is an integer and sig is shell-quoted.
	ec.Env = c.commandEnv()
	out, err := ec.CombinedOutput()
	if err != nil {
		return fmt.Errorf("signal %s pid %d in container %s: %w (output: %s)", sig, pid, c.Name, err, string(out))
	}
	return nil
}

// resolveMountPaths sets MountedPath for any repo that doesn't have one,
// using filepath.Base(GitRoot) by default. When any two repos share the
// same basename, all auto-populated repos switch to paths relative to the
// common parent directory of their GitRoots.
func resolveMountPaths(repos []Repo) error {
	// Track which repos were auto-populated (no explicit MountedPath).
	auto := make([]bool, len(repos))
	for i := range repos {
		auto[i] = repos[i].MountedPath == ""
		repos[i].populateMountPath()
	}

	// Detect basename conflicts.
	seen := make(map[string]struct{}, len(repos))
	hasConflict := false
	for _, r := range repos {
		if _, dup := seen[r.MountedPath]; dup {
			hasConflict = true
			break
		}
		seen[r.MountedPath] = struct{}{}
	}
	if !hasConflict {
		return nil
	}

	// Conflict detected: switch all auto-populated repos to relative paths
	// from their common parent directory.
	var autoDirs []string
	for i, r := range repos {
		if auto[i] {
			autoDirs = append(autoDirs, r.GitRoot)
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
		repos[i].MountedPath = "/home/user/src/" + filepath.ToSlash(rel)
	}

	// Final validation: check for remaining duplicate mount paths.
	final := make(map[string]int, len(repos))
	for i, r := range repos {
		if j, dup := final[r.MountedPath]; dup {
			return fmt.Errorf("repos[%d] and repos[%d] both mount as %q; set MountedPath to disambiguate", j, i, r.MountedPath)
		}
		final[r.MountedPath] = i
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

// Launch prepares the image and starts the Docker container. It does NOT
// wait for SSH to become ready — call Connect to complete startup once the
// container's repos have their branches set (e.g. after concurrent branch
// allocation).
func (c *Container) Launch(ctx context.Context, stdout, stderr io.Writer, opts *StartOpts) (retErr error) {
	// Resolve mount paths, disambiguating repos with the same basename
	// using relative paths. After this, all MountedPaths are unique.
	if err := resolveMountPaths(c.Repos); err != nil {
		return err
	}
	for i := range c.Repos {
		if err := c.Repos[i].resolveDefaults(ctx, c.Logger); err != nil {
			return fmt.Errorf("resolve defaults for %s: %w", c.Repos[i].MountedPath, err)
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
	// Validate git remotes before starting. Each remote must either be
	// absent (will be added) or point to the expected URL. A remote
	// pointing elsewhere indicates a name collision — fail early.
	for _, r := range c.Repos {
		rPath := r.MountedPath
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
	for _, repo := range c.Repos {
		if _, err := c.runCmd(ctx, repo.GitRoot, []string{"git", "remote", "get-url", c.Name}); err == nil {
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
				case deviceID == "":
					retErr = errors.Join(retErr, errors.New("tailscale node not removed: device ID unavailable"))
				default:
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

	for _, repo := range c.Repos {
		if _, err := c.runCmd(ctx, repo.GitRoot, []string{"git", "remote", "get-url", c.Name}); err == nil {
			if _, err := c.runCmd(ctx, repo.GitRoot, []string{"git", "remote", "remove", c.Name}); err != nil {
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
	if err := c.checkContainerState(ctx); err != nil {
		return "", err
	}
	r := &c.Repos[repoIdx]
	g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
	mp := shellQuote(r.MountedPath)
	backupBranch := "backup-" + time.Now().Format("20060102-150405")
	// Do the dirty check, optional commit, and backup branch creation in one
	// remote shell so every step observes the same index and HEAD.
	backupCommands := make([]string, 0, 5+2*len(r.Branches))
	backupCommands = append(backupCommands,
		"cd "+mp,
		"git add .",
		"diff_status=0",
		"{ git diff --quiet HEAD -- . || diff_status=$?; if [ \"$diff_status\" -gt 1 ]; then exit \"$diff_status\"; fi; if [ \"$diff_status\" -ne 0 ]; then git commit -q -m 'Backup before push'; fi; }",
	)
	backupCommands = append(backupCommands, backupContainerBranchesCommands(r.Branches, backupBranch)...)
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, strings.Join(backupCommands, " && ")), stdout, stderr); err != nil {
		return "", fmt.Errorf("backing up container state: %w", err)
	}
	// Refuse if there are pending local changes on any mapped branch.
	currentBranch, _ := g.RunGit(ctx, "branch", "--show-current")
	if slices.Contains(r.Branches, currentBranch) {
		if _, err := g.RunGit(ctx, "diff", "--quiet", "--exit-code"); err != nil {
			return "", fmt.Errorf("there are pending changes on branch %s locally. Please commit or stash them before pushing", currentBranch)
		}
	}
	bases, includeHost, err := c.pushMappedBranchRefs(ctx, stdout, stderr, r, true)
	if err != nil {
		return "", err
	}
	remoteURL, _ := c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "get-url", r.DefaultRemote})
	// Configure remotes before moving the branches; the branch resets depend on the
	// refs and upstreams set by the preceding push.
	if err := c.configureContainerRemotes(ctx, stdout, stderr, repoIdx, convertGitURLToHTTPS(remoteURL), includeHost, containerBranchSetupCommands(r.Branches, bases)...); err != nil {
		return "", err
	}
	// Update host's remote-tracking refs for all branches.
	for _, b := range r.Branches {
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "update-ref", "refs/remotes/" + c.Name + "/" + b, b}, stdout, stderr); err != nil {
			slog.WarnContext(ctx, "md", "msg", "failed to update host tracking ref", "branch", b, "err", err)
		}
	}
	return backupBranch, nil
}

// Fetch commits any uncommitted changes in Repos[repoIdx] in the container and
// fetches them locally, updating the remote-tracking ref without integrating.
//
// p controls AI commit message generation. Pass nil to use a default message.
func (c *Container) Fetch(ctx context.Context, stdout, stderr io.Writer, repoIdx int, p genai.Provider) error {
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	if repoIdx < 0 || repoIdx >= len(c.Repos) {
		return fmt.Errorf("repo index %d out of range [0, %d)", repoIdx, len(c.Repos))
	}
	if err := c.checkContainerState(ctx); err != nil {
		return err
	}
	r := &c.Repos[repoIdx]
	g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
	mp := shellQuote(r.MountedPath)
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return err
	}
	commitMsg := "Pull from md"
	gitUserName, _ := g.RunGit(ctx, "config", "user.name")
	gitUserEmail, _ := g.RunGit(ctx, "config", "user.email")
	if gitUserName == "" {
		gitUserName = "md"
	}
	if gitUserEmail == "" {
		gitUserEmail = "md@localhost"
	}
	gitAuthor := shellQuote(gitUserName + " <" + gitUserEmail + ">")
	if p == nil {
		// With the fixed commit message, one remote script can stage, check, and
		// commit. A clean tree exits successfully; real git errors still propagate.
		commitCmd := "cd " + mp + " && git add . && diff_status=0 && { git diff --quiet HEAD -- . || diff_status=$?; if [ \"$diff_status\" -eq 0 ]; then exit 0; fi; if [ \"$diff_status\" -gt 1 ]; then exit \"$diff_status\"; fi; echo " + shellQuote(commitMsg) + " | git commit -a -q --author " + gitAuthor + " -F -; }"
		if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, commitCmd), stdout, stderr); err != nil {
			return fmt.Errorf("committing in container: %w", err)
		}
	} else if _, err := c.runCmd(ctx, "", c.SSHCommand(nil, "cd "+mp+" && git add . && git diff --quiet HEAD -- .")); err != nil {
		metadata := c.gatherGitMetadata(ctx, r)
		diff := c.gatherGitDiff(ctx, r)
		if msg, err := git.GenerateCommitMsg(ctx, p, metadata, diff, nil); err != nil {
			c.Logger.Log(ctx, slog.LevelWarn, "failed to generate commit message", "err", err)
		} else if msg != "" {
			commitMsg = msg
		}
		commitCmd := "cd " + mp + " && echo " + shellQuote(commitMsg) + " | git commit -a -q --author " + gitAuthor + " -F -"
		if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, commitCmd), stdout, stderr); err != nil {
			return fmt.Errorf("committing in container: %w", err)
		}
	}
	// Fetch all mapped branches from the container.
	for _, b := range r.Branches {
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "fetch", "-q", c.Name, b}, stdout, stderr); err != nil {
			return fmt.Errorf("fetching %s: %w", b, err)
		}
	}
	return nil
}

// Pull fetches changes from the container and integrates all mapped branches
// into the local repository.
//
// p controls AI commit message generation. Pass nil to use a default message.
func (c *Container) Pull(ctx context.Context, stdout, stderr io.Writer, repoIdx int, p genai.Provider) error {
	if err := c.Fetch(ctx, stdout, stderr, repoIdx, p); err != nil {
		return err
	}
	r := &c.Repos[repoIdx]
	g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
	// Save the original branch to restore it after integration.
	origRef, _ := g.CurrentBranch(ctx)
	if origRef == "" {
		origRef, _ = g.RunGit(ctx, "rev-parse", "HEAD")
	}
	if err := c.pullBranches(ctx, stdout, stderr, r.GitRoot, r.Branches); err != nil {
		return err
	}
	// Restore the original branch.
	currentRef, _ := g.CurrentBranch(ctx)
	if origRef != "" && currentRef != origRef {
		if _, err := g.RunGit(ctx, "rev-parse", "--verify", "-q", origRef); err == nil {
			_ = c.runCmdOut(ctx, r.GitRoot, []string{"git", "checkout", "-q", origRef}, stdout, stderr)
		}
	}
	bases, includeHost, err := c.pushMappedBranchRefs(ctx, stdout, stderr, r, false)
	if err != nil {
		return err
	}
	remoteURL, _ := c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "get-url", r.DefaultRemote})
	// Configure remotes before moving the branches; the branch resets depend on the
	// refs and upstreams set by the preceding push.
	if err := c.configureContainerRemotes(ctx, stdout, stderr, repoIdx, convertGitURLToHTTPS(remoteURL), includeHost, containerBranchSetupCommands(r.Branches, bases)...); err != nil {
		return err
	}
	return nil
}

// Diff writes the diff between the host branch and current for Repos[repoIdx] to stdout/stderr.
// When stdout is a terminal, a TTY is allocated so git's pager and colors work.
func (c *Container) Diff(ctx context.Context, stdout, stderr io.Writer, repoIdx int, extraArgs []string) error {
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	if repoIdx < 0 || repoIdx >= len(c.Repos) {
		return fmt.Errorf("repo index %d out of range [0, %d)", repoIdx, len(c.Repos))
	}
	if err := c.checkContainerState(ctx); err != nil {
		return err
	}
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return err
	}
	repo := c.Repos[repoIdx]
	opts := []string{"-q"}
	isTTY := false
	if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		opts = append(opts, "-t")
		isTTY = true
	}
	exitOnDiff := slices.Contains(extraArgs, "--exit-code") || slices.Contains(extraArgs, "--quiet")
	sshArgs := c.SSHCommand(opts, gitDiffCommand(repo.MountedPath, extraArgs, exitOnDiff))
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "container", c.Name, "cmd", sshArgs)
	cmd := exec.CommandContext(ctx, sshArgs[0]) //nolint:gosec // args are from trusted SSH config
	cmd.Env = c.commandEnv()
	if isTTY {
		cmd.Stdin = os.Stdin
	}
	var err error
	cmd.Path, err = exec.LookPath(sshArgs[0])
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}
	cmd.Args = sshArgs
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func gitDiffCommand(repo string, extraArgs []string, exitOnDiff bool) string {
	diffArgs := ""
	if len(extraArgs) != 0 {
		quotedArgs := make([]string, len(extraArgs))
		for i, a := range extraArgs {
			quotedArgs[i] = shellQuote(a)
		}
		diffArgs = " " + strings.Join(quotedArgs, " ")
	}
	exitOnDiffFlag := "0"
	if exitOnDiff {
		exitOnDiffFlag = "1"
	}
	commands := []string{
		"cd " + shellQuote(repo),
		gitBaseRefCommand,
		"export GIT_OPTIONAL_LOCKS=0",
		`index_path=$(git rev-parse --git-path index) || exit $?`,
		`tmp_index=$(mktemp) || exit $?`,
		`untracked_paths=$(mktemp) || exit $?`,
		// Preserve the index timestamp so Git keeps its racy-clean checks valid.
		`cp -p "$index_path" "$tmp_index" || exit $?`,
		`trap 'rm -f "$tmp_index" "$untracked_paths"' EXIT`,
		`git ls-files -z --others --exclude-standard -- . > "$untracked_paths" || exit $?`,
		`while IFS= read -r -d '' path; do GIT_INDEX_FILE="$tmp_index" git add -N -- "$path" || exit $?; done < "$untracked_paths"`,
		"diff_status=0",
		`GIT_INDEX_FILE="$tmp_index" git diff "$diff_base_ref"` + diffArgs + ` -- . || diff_status=$?`,
		`if [ "$diff_status" -gt 1 ]; then exit "$diff_status"; fi`,
		"if [ " + exitOnDiffFlag + ` -eq 1 ]; then exit "$diff_status"; fi`,
	}
	return strings.Join(commands, "; ")
}

func forkRepoBranches(ctx context.Context, src *Repo, existing []*Container) ([]string, error) {
	usedBranches := map[string]struct{}{}
	for _, ct := range existing {
		for _, r := range ct.Repos {
			if r.GitRoot != src.GitRoot {
				continue
			}
			for _, branch := range r.Branches {
				usedBranches[branch] = struct{}{}
			}
		}
	}
	branches := slices.Clone(src.Branches)
	primary := src.Branches[0]
	for n := 0; ; n++ {
		candidate := fmt.Sprintf("%s-%d", primary, n)
		if _, ok := usedBranches[candidate]; ok {
			continue
		}
		exists, err := gitRefExists(ctx, src.GitRoot, "refs/heads/"+candidate)
		if err != nil {
			return nil, fmt.Errorf("checking fork branch %q: %w", candidate, err)
		}
		if !exists {
			branches[0] = candidate
			return branches, nil
		}
	}
}

// Fork snapshots a running container and creates a new one where each mapped
// repository is checked out on a new primary branch.
//
// The snapshot preserves the container's entire filesystem (installed
// packages, build artifacts, etc.) while giving each repo a fresh primary
// branch that diverges from the source container's working state. Extra mapped
// branches keep their original names.
//
// Branch naming: the primary branch gets a unique destination branch derived
// from its source branch (e.g. "main" → "main-0").
func (c *Container) Fork(ctx context.Context, stdout, stderr io.Writer, opts *ForkOpts) (*Container, error) {
	if err := c.checkContainerState(ctx); err != nil {
		return nil, err
	}
	// Validate that extra repos don't overlap with source repos.
	sourceRoots := make(map[string]struct{}, len(c.Repos))
	for _, r := range c.Repos {
		sourceRoots[r.GitRoot] = struct{}{}
	}
	for _, r := range opts.ExtraRepos {
		if _, ok := sourceRoots[r.GitRoot]; ok {
			return nil, fmt.Errorf("extra repo %s already exists in source container", r.GitRoot)
		}
	}

	// Resolve extra repos: default Branches to the repo's default upstream branch.
	extraRepos := slices.Clone(opts.ExtraRepos)
	for i := range extraRepos {
		if len(extraRepos[i].Branches) == 0 {
			if err := extraRepos[i].resolveDefaults(ctx, c.Logger); err != nil {
				return nil, fmt.Errorf("resolving defaults for extra repo %s: %w", extraRepos[i].GitRoot, err)
			}
			extraRepos[i].Branches = []string{extraRepos[i].DefaultBranch}
		}
		if err := extraRepos[i].Validate(); err != nil {
			return nil, fmt.Errorf("extra repo %s: %w", extraRepos[i].GitRoot, err)
		}
	}

	// Generate a unique destination primary branch for each mapped repo.
	allSrc := append(slices.Clone(c.Repos), extraRepos...)
	forkRepos := slices.Clone(allSrc)
	existing, err := c.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers for fork branch allocation: %w", err)
	}
	for i := range allSrc {
		branches, err := forkRepoBranches(ctx, &allSrc[i], existing)
		if err != nil {
			return nil, fmt.Errorf("allocating fork branches for %s: %w", allSrc[i].GitRoot, err)
		}
		forkRepos[i].Branches = branches
	}

	// Snapshot the source container, stripping all labels so
	// launchContainer sets them fresh on the forked container.
	// docker commit bakes container labels into the image; any label
	// not explicitly re-set by launchContainer would leak through.
	snapshotImage := "md-fork-" + c.Name
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

	// Create the new container handle with destination branches.
	fork, err := c.Container(forkRepos...)
	if err != nil {
		return nil, fmt.Errorf("fork container: %w", err)
	}

	// Fetch current state from source container and create/reset local branches
	// for repos inherited from the source.
	if !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- Creating local branches ...")
	}
	for i, r := range c.Repos {
		g := &git.Checkout{Root: r.GitRoot, Logger: c.Logger}
		curr, _ := g.CurrentBranch(ctx)
		if err := c.runCmdOut(ctx, r.GitRoot, append([]string{"git", "fetch", "-q", c.Name}, r.Branches...), stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from source container: %w", r.MountedPath, err)
		}
		fetchedRef := c.Name + "/" + r.Branches[0]
		newBranch := fork.Repos[i].Branches[0]
		if curr == newBranch {
			if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "reset", "--hard", fetchedRef}, stdout, stderr); err != nil {
				return nil, fmt.Errorf("resetting branch %s: %w", newBranch, err)
			}
		} else if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "branch", "-f", newBranch, fetchedRef}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("creating branch %s: %w", newBranch, err)
		}
	}

	// Start the new container from the snapshot image.
	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Starting forked container %s ...\n", fork.Name)
	}
	startOpts := opts.startOptions()
	fork.prepareTailscaleAuthKey(ctx, stdout, startOpts)
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
	fork.Display = startOpts.Display
	fork.Tailscale = startOpts.Tailscale
	fork.USB = startOpts.USB
	fork.Sudo = startOpts.Sudo

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
	sshCommandDeadline := time.Now().Add(containerSSHCommandRetryTimeout)

	// Send .env into the forked container.
	var envContent []byte
	for _, r := range forkRepos {
		data, err := os.ReadFile(filepath.Join(r.GitRoot, ".env"))
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
	fork.Logger.Log(ctx, slog.LevelDebug, "ssh", "container", fork.Name, "cmd", sshEnvArgs)
	for {
		cmd := exec.CommandContext(ctx, sshEnvArgs[0], sshEnvArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Env = fork.commandEnv()
		cmd.Stdin = bytes.NewReader(envContent)
		out, err := cmd.CombinedOutput()
		if err == nil {
			break
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 || time.Now().After(sshCommandDeadline) {
			return nil, fmt.Errorf("copying .env to forked container: %w\n%s", err, out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Restore ownership before pushing. Under rootless podman, `commit` does
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

	// Inside the forked container: rename source repo branches and push extra
	// repos as new.
	if !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- Setting up branches in forked container ...")
	}
	for i, r := range c.Repos {
		refspecs := make([]string, len(fork.Repos[i].Branches))
		setupCommands := []string{"cd " + shellQuote(r.MountedPath), hostRemoteSetupCommand}
		oldPrimary := r.Branches[0]
		newPrimary := fork.Repos[i].Branches[0]
		refspecs[0] = newPrimary + ":refs/remotes/host/" + newPrimary
		setupCommands = append(setupCommands,
			forkPrimaryBranchSetupCommand(oldPrimary, newPrimary),
			"git branch -q --set-upstream-to="+shellQuote("host/"+newPrimary)+" "+shellQuote(newPrimary),
		)
		for j, branch := range r.Branches[1:] {
			refspecs[j+1] = c.Name + "/" + branch + ":refs/remotes/host/" + branch
			setupCommands = append(setupCommands, "git branch -q --set-upstream-to="+shellQuote("host/"+branch)+" "+shellQuote(branch))
		}
		args := append([]string{"git", "push", "-q", "-f", fork.Name}, refspecs...)
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, args, stdout, stderr); err != nil {
			return nil, fmt.Errorf("pushing fork branches for %s: %w", r.MountedPath, err)
		}
		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, strings.Join(setupCommands, " && ")), stdout, stderr); err != nil {
			return nil, fmt.Errorf("setting up branches for %s: %w", r.MountedPath, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{"git", "fetch", "-q", fork.Name, newPrimary}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from fork: %w", newPrimary, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{"git", "branch", "--set-upstream-to", fork.Name + "/" + newPrimary, newPrimary}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("setting upstream for %s: %w", newPrimary, err)
		}
	}
	// Push extra repos into the container using source branches, set up
	// destination branches inside.
	nSrc := len(c.Repos)
	for i, src := range extraRepos {
		dst := forkRepos[nSrc+i]
		mp := shellQuote(src.MountedPath)

		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, "git init -q "+mp+" && git -C "+mp+" remote add host /dev/null"), stdout, stderr); err != nil {
			return nil, fmt.Errorf("init extra repo %s in container: %w", src.MountedPath, err)
		}
		refspecs := make([]string, len(src.Branches))
		bases := make([]containerBranchBase, len(dst.Branches))
		for j, branch := range src.Branches {
			refspecs[j] = branch + ":refs/remotes/host/" + dst.Branches[j]
			bases[j] = containerBranchBase{ref: "host/" + dst.Branches[j]}
		}
		args := append([]string{"git", "push", "-q", fork.Name}, refspecs...)
		if err := c.runCmdOut(ctx, src.GitRoot, args, stdout, stderr); err != nil {
			return nil, fmt.Errorf("push extra repo %s: %w", src.MountedPath, err)
		}
		setupCommands := append([]string{"cd " + mp, hostRemoteSetupCommand}, containerBranchSetupCommands(dst.Branches, bases)...)
		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, strings.Join(setupCommands, " && ")), stdout, stderr); err != nil {
			return nil, fmt.Errorf("setting up extra repo %s: %w", src.MountedPath, err)
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
	return strings.Join([]string{
		"old_ref=" + shellQuote(oldRef),
		"new_ref=" + shellQuote(newRef),
		"rebase_head=",
		`for candidate in "$(git rev-parse --git-path rebase-merge/head-name)" "$(git rev-parse --git-path rebase-apply/head-name)"; do if [ -s "$candidate" ] && [ "$(cat "$candidate")" = "$old_ref" ]; then rebase_head=$candidate; break; fi; done`,
		`if [ -n "$rebase_head" ]; then git update-ref "$new_ref" "$old_ref" "" && printf '%s\n' "$new_ref" > "$rebase_head" && git branch -q -D ` + shellQuote(oldBranch) + `; else git branch -m ` + shellQuote(oldBranch) + ` ` + shellQuote(newBranch) + `; fi`,
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
		c.Logger.Log(ctx, slog.LevelDebug, "tailscale status failed", "container", c.Name, "err", err)
		return ""
	}
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		c.Logger.Log(ctx, slog.LevelDebug, "tailscale status JSON parse failed", "container", c.Name, "err", err)
		return ""
	}
	fqdn := strings.TrimRight(status.Self.DNSName, ".")
	if fqdn == "" {
		c.Logger.Log(ctx, slog.LevelDebug, "tailscale FQDN empty", "container", c.Name)
	}
	return fqdn
}

// SyncDefaultBranch force-pushes the host's default branch (e.g. origin/main)
// for Repos[repoIdx] into the container's remote-tracking refs. It also
// refreshes upstream refs for mapped branches so agents can diff against branch upstreams.
func (c *Container) SyncDefaultBranch(ctx context.Context, repoIdx int) error {
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	refspecs, err := c.Repos[repoIdx].containerSyncRefspecs(ctx, c.Logger)
	if err != nil {
		return err
	}
	if err := c.pushContainerRefs(ctx, &c.Repos[repoIdx], refspecs); err != nil {
		return fmt.Errorf("sync refs %q: %w", refspecs, err)
	}
	return nil
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

func (c *Container) configureContainerRemotes(ctx context.Context, stdout, stderr io.Writer, repoIdx int, remoteURL string, includeHost bool, postCommands ...string) error {
	// postCommands run after the remote config in the same remote shell. Push,
	// Pull, and provisioning use this to express the full ordered git transition
	// at one call site.
	r := &c.Repos[repoIdx]
	commands := containerRemoteConfigCommands(r, remoteURL, includeHost)
	commands = append(commands, postCommands...)
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, strings.Join(commands, " && ")), stdout, stderr); err != nil {
		return fmt.Errorf("configuring remotes for %s: %w", r.MountedPath, err)
	}
	return nil
}

func containerRemoteConfigCommands(r *Repo, remoteURL string, includeHost bool) []string {
	commands := []string{"cd " + shellQuote(r.MountedPath)}
	if includeHost {
		commands = append(commands, hostRemoteSetupCommand)
	}
	remoteConfigPrefix := "remote." + r.DefaultRemote
	if remoteURL != "" {
		commands = append(commands, "git config --replace-all "+shellQuote(remoteConfigPrefix+".url")+" "+shellQuote(remoteURL))
	}
	commands = append(commands, "git config --replace-all "+shellQuote(remoteConfigPrefix+".fetch")+" "+shellQuote("+refs/heads/*:"+remoteTrackingRef(r.DefaultRemote, "*")))
	return commands
}

func (c *Container) pushContainerRefs(ctx context.Context, r *Repo, refspecs []string) error {
	if len(refspecs) == 0 {
		return nil
	}
	args := append([]string{"git", "push", "-q", "-f", c.Name}, refspecs...)
	_, err := c.runCmd(ctx, r.GitRoot, args)
	return err
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

// pushMappedBranchRefs pushes all mapped host branches and their diff-base refs
// into the container remote.
//
// It returns one base per mapped branch for configuring the container working
// tree, and whether the fake host remote must be configured because at least
// one branch uses refs/remotes/host as its diff base.
func (c *Container) pushMappedBranchRefs(ctx context.Context, stdout, stderr io.Writer, r *Repo, includeTags bool) ([]containerBranchBase, bool, error) {
	bases := make([]containerBranchBase, len(r.Branches))
	includeHost := false
	refspecs := make([]string, 0, len(r.Branches))
	for i, b := range r.Branches {
		base, err := r.resolveContainerBranchBase(ctx, c.Logger, b)
		if err != nil {
			return nil, false, fmt.Errorf("resolve branch base for %s: %w", b, err)
		}
		bases[i] = base
		includeHost = includeHost || base.useHost
		refspecs = appendUniqueRefspecs(refspecs, base.source+":"+base.destination)
	}
	syncRefspecs, err := r.containerSyncRefspecs(ctx, c.Logger)
	if err != nil {
		return nil, false, err
	}
	refspecs = appendUniqueRefspecs(refspecs, syncRefspecs...)

	args := []string{"git", "push", "-q", "-f"}
	if includeTags {
		args = append(args, "--tags")
	}
	args = append(args, c.Name)
	args = append(args, refspecs...)
	if err := c.runCmdOut(ctx, r.GitRoot, args, stdout, stderr); err != nil {
		return nil, false, fmt.Errorf("push mapped branches: %w", err)
	}
	return bases, includeHost, nil
}

// containerBranchSetupCommands returns shell commands that create/reset mapped
// branches inside the container from their selected base refs and configure each
// branch to track that base.
func containerBranchSetupCommands(branches []string, bases []containerBranchBase) []string {
	commands := make([]string, 0, 2*len(branches))
	for i, b := range branches {
		branch := shellQuote(b)
		baseRef := shellQuote(bases[i].ref)
		if i == 0 {
			commands = append(commands, "git switch -q -C "+branch+" "+baseRef)
		} else {
			commands = append(commands, "git branch -q -f "+branch+" "+baseRef)
		}
		commands = append(commands, "git branch -q --set-upstream-to="+baseRef+" "+branch)
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
	source      string
	ref         string
	useHost     bool
	destination string
}

func defaultBranchPushSource(ctx context.Context, r *Repo) (src string, ok bool, err error) {
	remoteRef := remoteTrackingRef(r.DefaultRemote, r.DefaultBranch)
	ok, err = gitRefExists(ctx, r.GitRoot, remoteRef)
	if err != nil {
		return "", false, err
	}
	if ok {
		return remoteRef, true, nil
	}
	localRef := "refs/heads/" + r.DefaultBranch
	ok, err = gitRefExists(ctx, r.GitRoot, localRef)
	if err != nil {
		return "", false, err
	}
	if ok {
		return localRef, true, nil
	}
	return "", false, nil
}

func gitRefExists(ctx context.Context, dir, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref) //nolint:gosec // ref is passed as one argument.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LANG=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git show-ref --verify --quiet %s: %w: %s", ref, err, stderr.String())
	}
	return true, nil
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

	dst := filepath.Join(tmpDir, "file")
	if _, err := c.Runtime.Run(ctx, "", "cp", c.Name+":"+containerPath, dst); err != nil {
		return "", err
	}
	data, err2 := os.ReadFile(dst) // #nosec G304 -- dst is a private temp file populated by docker cp.
	if err2 != nil {
		return "", err2
	}
	return string(data), nil
}

// pullBranches integrates mapped branches from the container's remote-tracking
// refs into the host's local branches.
func (c *Container) pullBranches(ctx context.Context, stdout, stderr io.Writer, gitRoot string, branches []string) error {
	commands := make([]string, 0, len(branches))
	for _, branch := range branches {
		quotedBranch := shellQuote(branch)
		localRef := shellQuote("refs/heads/" + branch)
		remoteRef := shellQuote(c.Name + "/" + branch)
		commands = append(commands,
			"current_branch=$(git branch --show-current || true); if ! git show-ref --verify --quiet "+localRef+"; then git update-ref "+localRef+" "+remoteRef+"; elif [ \"$current_branch\" = "+quotedBranch+" ]; then git rebase -q "+remoteRef+"; elif git merge-base --is-ancestor "+quotedBranch+" "+remoteRef+"; then git update-ref "+localRef+" "+remoteRef+"; else git checkout -q "+quotedBranch+" && git rebase -q "+remoteRef+"; fi",
		)
	}
	if err := c.runCmdOut(ctx, gitRoot, []string{"bash", "-c", strings.Join(commands, " && ")}, stdout, stderr); err != nil {
		return fmt.Errorf("integrating mapped branches: %w", err)
	}
	return nil
}

// waitForSSH runs a trivial SSH command in a retry loop until it succeeds or
// the deadline is exceeded. This confirms SSH is fully operational after the
// TCP socket opens (sshd may need a few more milliseconds to accept auth).
func (c *Container) waitForSSH(ctx context.Context, deadline time.Time) error {
	var lastErr error
	sshArgs := c.SSHCommand(nil, "true")
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "container", c.Name, "cmd", sshArgs)
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
	repo := shellQuote(r.MountedPath)
	cmd := "cd " + repo + " && " + gitBaseRefCommand + " && echo '=== Branch ===' && git rev-parse --abbrev-ref HEAD && echo && echo '=== Files Changed ===' && git diff --stat --cached \"$diff_base_ref\" -- . && echo && echo '=== Recent Commits ===' && git log -5 \"$base_ref\" -- ."
	out, _ := c.runCmd(ctx, "", c.SSHCommand(nil, cmd))
	return out
}

// gatherGitDiff runs SSH to get the full patience diff from the container.
func (c *Container) gatherGitDiff(ctx context.Context, r *Repo) string {
	repo := shellQuote(r.MountedPath)
	cmd := "cd " + repo + " && " + gitBaseRefCommand + " && git diff --patience -U10 --cached \"$diff_base_ref\" -- ."
	out, _ := c.runCmd(ctx, "", c.SSHCommand(nil, cmd))
	return out
}

func (c *Container) checkContainerState(ctx context.Context) error {
	_, containerErr := c.Runtime.Run(ctx, "", "inspect", c.Name)
	containerExists := containerErr == nil
	var remoteExists bool
	if len(c.Repos) > 0 {
		_, remoteErr := c.runCmd(ctx, c.Repos[0].GitRoot, []string{"git", "remote", "get-url", c.Name})
		remoteExists = remoteErr == nil
	}
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	_, sshErr := os.Stat(filepath.Join(sshConfigDir, c.Name+".conf"))
	sshExists := sshErr == nil

	if !containerExists && !remoteExists && !sshExists {
		if len(c.Repos) > 0 {
			return fmt.Errorf("no container running for branch '%s'.\nStart a container with: md start", c.Repos[0].Branches[0])
		}
		return fmt.Errorf("container %s not found.\nStart a container with: md start", c.Name)
	}
	var issues []string
	if !containerExists {
		issues = append(issues, "Docker container is not running")
	}
	if len(c.Repos) > 0 && !remoteExists {
		issues = append(issues, "Git remote is missing")
	}
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

// pushSubmodules transfers submodule bare repos from hostGitRoot into the
// container at containerRepoPath and initializes them at all nesting depths
// without requiring network access. containerRepoPath is the absolute path
// inside the container (e.g. /home/user/src/myrepo).
//
// Returns nil when no submodules are declared or when submodules are declared
// but not yet cloned on the host (uninitialized).
func (c *Container) pushSubmodules(ctx context.Context, stdout, stderr io.Writer, containerRepoPath, hostGitRoot string, quiet bool) error {
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
		// Push all refs from host bare module repo to container.
		// Use GIT_DIR env var instead of --git-dir because --git-dir
		// still reads core.worktree from the repo config and tries
		// to chdir there, which fails when the submodule worktree
		// was never checked out (init but not update, or deinited).
		// GIT_DIR fully decouples git from any worktree.
		containerURL := "user@" + c.Name + ":" + containerModuleDir
		if err := c.runGitDir(ctx, hostGitRoot, hostModuleDir, "push", "-q", containerURL, "--all"); err != nil {
			return fmt.Errorf("push submodule refs %s: %w", relPath, err)
		}
		if err := c.runGitDir(ctx, hostGitRoot, hostModuleDir, "push", "-q", containerURL, "--tags"); err != nil {
			return fmt.Errorf("push submodule tags %s: %w", relPath, err)
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
		// a virtual /etc/localtime inside the VM, so the flag is universal.
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

	// Rootless podman: --userns=keep-id maps host UID to same UID inside the
	// container so bind-mounted configs are writable. --user 0:0 keeps
	// start.sh running as root for privileged setup (groupmod, sshd, dbus).
	// Rootless Docker is handled inside start.sh via /proc/self/uid_map
	// detection since Docker lacks --userns=keep-id.
	//
	// Trade-off: keep-id ownership does not round-trip through `podman
	// commit`, so Fork must re-chown snapshotted repos. See docs/ROOTLESS.md.
	if c.Runtime.IsRootless() {
		runArgs = append(runArgs, "--userns=keep-id", "--user", "0:0")
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
	// VM that cannot access host USB devices). Use a bind mount + cgroup
	// rule so that devices plugged in after container start are visible.
	if opts.USB {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("--usb requires Linux; Docker Desktop on %s cannot pass through host USB devices", runtime.GOOS)
		}
		runArgs = append(runArgs,
			"-v", "/dev/bus/usb:/dev/bus/usb",
			"--device-cgroup-rule=c 189:* rwm")
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
			return cmdErrWithStderr("starting container", err)
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "- Starting container %s ... ", c.Name)
		if err := c.Runtime.RunOut(ctx, "", stdout, stderr, runArgs...); err != nil {
			_, _ = fmt.Fprintln(stdout)
			return fmt.Errorf("starting container: %w", err)
		}
	}

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
		for _, r := range c.Repos {
			rPath := r.MountedPath
			_, _ = c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "rm", c.Name})
			if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "remote", "add", c.Name, "user@" + c.Name + ":" + rPath}, stdout, stderr); err != nil {
				return fmt.Errorf("adding git remote for %s: %w", rPath, err)
			}
		}
	}
	return nil
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
		url, err := c.tryReadTailscaleAuthURL(ctx, stdout)
		if err != nil {
			return nil, fmt.Errorf("reading Tailscale auth URL: %w", err)
		}
		result.TailscaleAuthURL = url
	}

	// Phase 1: wait for SSH to accept connections.
	addr := fmt.Sprintf("localhost:%d", c.SSHPort)
	if err := waitForTCP(ctx, addr, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return nil, err
	}
	if err := c.waitForSSH(ctx, time.Now().Add(containerSSHReadyTimeout)); err != nil {
		return nil, err
	}

	// Phase 2: push all repos into the container in parallel, including
	// submodules. Each repo pushes to a distinct path (~/src/<name>).
	//
	// Inside the container, a fake "host" remote (URL /dev/null) holds
	// host-side branches as refs/remotes/host/<name>. The agent's working
	// branch is checked out from and tracks host/<primary>.
	if len(c.Repos) > 0 {
		if !opts.Quiet {
			_, _ = fmt.Fprintln(stdout, "- git clone into container ...")
		}
		eg, egCtx := errgroup.WithContext(ctx)
		for repoIdx := range c.Repos {
			eg.Go(func() error {
				r := &c.Repos[repoIdx]
				mp := shellQuote(r.MountedPath)

				if err := c.runCmdOut(egCtx, "", c.SSHCommand(nil, "git init -q "+mp+" && git -C "+mp+" remote add host /dev/null"), stdout, stderr); err != nil {
					return fmt.Errorf("init repo %s in container: %w", r.MountedPath, err)
				}
				if err := r.resolveDefaults(egCtx, c.Logger); err != nil {
					return fmt.Errorf("resolve defaults for %s: %w", r.MountedPath, err)
				}
				bases, includeHost, err := c.pushMappedBranchRefs(egCtx, stdout, stderr, r, false)
				if err != nil {
					return fmt.Errorf("push repo %s: %w", r.MountedPath, err)
				}
				remoteURL, _ := c.runCmd(egCtx, r.GitRoot, []string{"git", "remote", "get-url", r.DefaultRemote})
				httpsURL := convertGitURLToHTTPS(remoteURL)
				if !opts.Quiet && httpsURL != "" {
					_, _ = fmt.Fprintf(stdout, "- Set %s %s to %s\n", r.MountedPath, r.DefaultRemote, httpsURL)
				}
				if err := c.configureContainerRemotes(egCtx, stdout, stderr, repoIdx, httpsURL, includeHost, containerBranchSetupCommands(r.Branches, bases)...); err != nil {
					return err
				}

				if err := c.pushSubmodules(egCtx, stdout, stderr, r.MountedPath, r.GitRoot, opts.Quiet); err != nil {
					return fmt.Errorf("push submodules for %s: %w", r.MountedPath, err)
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
	for _, r := range c.Repos {
		data, err := os.ReadFile(filepath.Join(r.GitRoot, ".env"))
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
	c.Logger.Log(ctx, slog.LevelDebug, "ssh", "container", c.Name, "cmd", sshEnvArgs)
	cmd := exec.CommandContext(ctx, sshEnvArgs[0], sshEnvArgs[1:]...) //nolint:gosec // args are from trusted SSH config
	cmd.Env = c.commandEnv()
	cmd.Stdin = bytes.NewReader(envContent)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("copying .env: %w\n%s", err, out)
	}
	return nil
}

// convertGitURLToHTTPS converts a git URL to HTTPS format.
//
// Supports git@host:path, ssh://git@host/path, git://host/path, and
// https:// (returned unchanged).
func convertGitURLToHTTPS(url string) string {
	url = strings.TrimSpace(url)
	if strings.HasPrefix(url, "https://") {
		return url
	}
	// Matches git@host:user/repo.git
	if m := reGitAt.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	// Matches ssh://git@host/user/repo.git
	if m := reSSHGit.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	// Matches git://host/user/repo.git
	if m := reGitProto.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	return url
}

// parsePSOutput parses ps output. The last column (args) may contain spaces;
// the first seven fields are whitespace-separated and the remainder is the command.
func parsePSOutput(out string) ([]ProcessInfo, error) {
	var procs []ProcessInfo
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 8)
		var clean []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				clean = append(clean, p)
			}
		}
		if len(clean) < 8 {
			clean = strings.Fields(line)
			if len(clean) < 8 {
				continue
			}
			cmd := strings.Join(clean[7:], " ")
			clean = append(clean[:7], cmd)
		}
		pid, err := strconv.Atoi(clean[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(clean[1])
		cpu, _ := strconv.ParseFloat(clean[4], 64)
		mem, _ := strconv.ParseFloat(clean[5], 64)
		procs = append(procs, ProcessInfo{
			PID:     pid,
			PPID:    ppid,
			User:    clean[2],
			State:   clean[3],
			CPU:     cpu,
			Mem:     mem,
			Time:    clean[6],
			Command: clean[7],
		})
	}
	return slices.DeleteFunc(procs, func(p ProcessInfo) bool {
		return strings.HasPrefix(p.Command, "ps ")
	}), nil
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

// psPorts handles Docker's string format and Podman's array format for the
// Ports field in `docker ps --format '{{json .}}'`.
type psPorts string

type podmanPortMapping struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Proto         string `json:"proto"`
}

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
					client.Logger.Log(ctx, slog.LevelWarn, "failed to unmarshal repos label", "err", err)
				}
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
