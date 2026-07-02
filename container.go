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

	"github.com/maruel/genai"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"github.com/caic-xyz/md/gitutil"
)

// DefaultBaseImage is the base image used when none is specified.
const DefaultBaseImage = "ghcr.io/caic-xyz/md-user"

const (
	tailscaleDeviceIDPath = "/var/lib/md/tailscale_device_id"

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
	// Branch is the git branch to push into the container.
	Branch string `json:"branch"`
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

// Validate returns an error for invalid repo fields.
func (r *Repo) Validate() error {
	r.populateMountPath()
	r.MountedPath = ResolveContainerPath(r.MountedPath)
	if r.GitRoot == "" {
		return errors.New("Repo.GitRoot is empty")
	}
	if r.MountedPath == "" {
		return errors.New("Repo.MountedPath could not be determined from GitRoot")
	}
	if !path.IsAbs(r.MountedPath) {
		return fmt.Errorf("Repo.MountedPath must be an absolute POSIX path, got %q", r.MountedPath)
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

// resolveDefaults populates DefaultRemote and DefaultBranch if not already set.
func (r *Repo) resolveDefaults(ctx context.Context) error {
	if r.DefaultRemote == "" {
		remote, err := gitutil.DefaultRemote(ctx, r.GitRoot)
		if err != nil {
			return err
		}
		r.DefaultRemote = remote
	}
	if r.DefaultBranch == "" {
		branch, err := gitutil.DefaultBranch(ctx, r.GitRoot, r.DefaultRemote)
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
		if err := c.Repos[i].resolveDefaults(ctx); err != nil {
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
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name}); err == nil {
		var suffix [4]byte
		_, _ = rand.Read(suffix[:])
		c.Name = c.Name + "-" + hex.EncodeToString(suffix[:])
		if _, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name}); err == nil {
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
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "start", c.Name}); err != nil {
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
	removeSSHConfig(ctx, c.Client, sshConfigDir, c.Name)
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
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "stop", c.Name}); err != nil {
		return fmt.Errorf("docker stop %s: %w", c.Name, err)
	}
	// Clean up stale ControlMaster socket (if any). The SSH connection is
	// dead now that the container is stopped.
	cleanupControlSocket(ctx, c.Client, c.Name)
	c.State = "exited"
	return nil
}

// Purge stops and removes the container, cleaning up SSH config and git remotes.
func (c *Container) Purge(ctx context.Context, stdout, stderr io.Writer) error {
	_, containerErr := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name})
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
			tsLabel, _ := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", `{{index .Config.Labels "md.tailscale"}}`, c.Name})
			c.Tailscale = tsLabel == "1"
		}
		if c.Tailscale {
			ephLabel, _ := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", `{{index .Config.Labels "md.tailscale_ephemeral"}}`, c.Name})
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

	_ = os.Remove(sshConf)
	_ = os.Remove(sshKnown)

	for _, repo := range c.Repos {
		if _, err := c.runCmd(ctx, repo.GitRoot, []string{"git", "remote", "get-url", c.Name}); err == nil {
			if _, err := c.runCmd(ctx, repo.GitRoot, []string{"git", "remote", "remove", c.Name}); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}
	if containerExists {
		if _, err := c.runCmd(ctx, "", []string{c.Runtime, "rm", "-f", "-v", c.Name}); err != nil {
			retErr = err
		}
	}
	_, _ = fmt.Fprintf(stdout, "Removed %s\n", c.Name)
	return retErr
}

// Push force-pushes local state for Repos[repoIdx] into the container,
// saving a backup of the container state and returning the backup branch name.
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
	mp := shellQuote(r.MountedPath)
	branch := shellQuote(r.Branch)
	backupBranch := "backup-" + time.Now().Format("20060102-150405")
	// Do the dirty check, optional commit, and backup branch creation in one
	// remote shell so every step observes the same index and HEAD.
	backupCmd := "cd " + mp + " && git add . && diff_status=0 && { git diff --quiet HEAD -- . || diff_status=$?; if [ \"$diff_status\" -gt 1 ]; then exit \"$diff_status\"; fi; if [ \"$diff_status\" -ne 0 ]; then git commit -q -m 'Backup before push'; fi; git branch -f " + shellQuote(backupBranch) + " HEAD; }"
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, backupCmd), stdout, stderr); err != nil {
		return "", fmt.Errorf("backing up container state: %w", err)
	}
	// Refuse if there are pending local changes on the branch being pushed.
	currentBranch, _ := gitutil.RunGit(ctx, r.GitRoot, "branch", "--show-current")
	if currentBranch == r.Branch {
		if _, err := gitutil.RunGit(ctx, r.GitRoot, "diff", "--quiet", "--exit-code"); err != nil {
			return "", errors.New("there are pending changes locally. Please commit or stash them before pushing")
		}
	}
	base, err := r.resolveContainerBranchBase(ctx)
	if err != nil {
		return "", err
	}
	// Push the task branch and the refs used as diff bases together. Git accepts
	// several refspecs in one invocation.
	syncRefspecs, err := r.containerSyncRefspecs(ctx)
	if err != nil {
		return "", err
	}
	refspecs := appendUniqueRefspecs([]string{base.source + ":" + base.destination}, syncRefspecs...)
	pushArgs := append([]string{"git", "push", "-q", "-f", "--tags", c.Name}, refspecs...)
	if err := c.runCmdOut(ctx, r.GitRoot, pushArgs, stdout, stderr); err != nil {
		return "", err
	}
	remoteURL, _ := c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "get-url", r.DefaultRemote})
	baseRef := shellQuote(base.ref)
	// Configure remotes before moving the branch; the branch reset depends on the
	// refs and upstreams set by the preceding commands.
	if err := c.configureContainerRemotes(ctx, stdout, stderr, repoIdx, convertGitURLToHTTPS(remoteURL), base.useHost,
		"git switch -q -C "+branch+" "+baseRef,
		"git branch -q --set-upstream-to="+baseRef); err != nil {
		return "", err
	}
	// Update the local remote-tracking ref so it reflects the pushed state.
	if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "update-ref", "refs/remotes/" + c.Name + "/" + r.Branch, r.Branch}, stdout, stderr); err != nil {
		return "", err
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
	mp := shellQuote(r.MountedPath)
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return err
	}
	commitMsg := "Pull from md"
	gitUserName, _ := gitutil.RunGit(ctx, r.GitRoot, "config", "user.name")
	gitUserEmail, _ := gitutil.RunGit(ctx, r.GitRoot, "config", "user.email")
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
		if msg, err := gitutil.GenerateCommitMsg(ctx, p, metadata, diff, nil); err != nil {
			c.Logger.Log(ctx, slog.LevelWarn, "failed to generate commit message", "err", err)
		} else if msg != "" {
			commitMsg = msg
		}
		commitCmd := "cd " + mp + " && echo " + shellQuote(commitMsg) + " | git commit -a -q --author " + gitAuthor + " -F -"
		if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, commitCmd), stdout, stderr); err != nil {
			return fmt.Errorf("committing in container: %w", err)
		}
	}
	if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "fetch", "-q", c.Name, r.Branch}, stdout, stderr); err != nil {
		return err
	}
	return nil
}

// Pull fetches changes from the container and integrates Repos[repoIdx] into
// the local branch.
//
// p controls AI commit message generation. Pass nil to use a default message.
func (c *Container) Pull(ctx context.Context, stdout, stderr io.Writer, repoIdx int, p genai.Provider) error {
	if err := c.Fetch(ctx, stdout, stderr, repoIdx, p); err != nil {
		return err
	}
	r := &c.Repos[repoIdx]
	remoteRef := c.Name + "/" + r.Branch
	branchExists, err := gitRefExists(ctx, r.GitRoot, "refs/heads/"+r.Branch)
	if err != nil {
		return err
	}
	currentBranch, _ := gitutil.RunGit(ctx, r.GitRoot, "branch", "--show-current")
	if !branchExists {
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "update-ref", "refs/heads/" + r.Branch, remoteRef}, stdout, stderr); err != nil {
			return err
		}
	} else if currentBranch == r.Branch {
		// Already on the branch, rebase locally.
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "rebase", "-q", remoteRef}, stdout, stderr); err != nil {
			return err
		}
	} else if _, err := gitutil.RunGit(ctx, r.GitRoot, "merge-base", "--is-ancestor", r.Branch, remoteRef); err == nil {
		// Fast-forward: update ref without checkout.
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "update-ref", "refs/heads/" + r.Branch, remoteRef}, stdout, stderr); err != nil {
			return err
		}
	} else {
		// Not a fast-forward. Checkout the branch, rebase, then checkout back.
		origRef := currentBranch
		if origRef == "" {
			origRef, _ = gitutil.RunGit(ctx, r.GitRoot, "rev-parse", "HEAD")
		}
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "checkout", "-q", r.Branch}, stdout, stderr); err != nil {
			return err
		}
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "rebase", "-q", remoteRef}, stdout, stderr); err != nil {
			_ = c.runCmdOut(ctx, r.GitRoot, []string{"git", "checkout", "-q", origRef}, stdout, stderr)
			return err
		}
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "checkout", "-q", origRef}, stdout, stderr); err != nil {
			return err
		}
	}
	base, err := r.resolveContainerBranchBase(ctx)
	if err != nil {
		return err
	}
	pushArgs := []string{"git", "push", "-q", "-f", c.Name, base.source + ":" + base.destination}
	if err := c.runCmdOut(ctx, r.GitRoot, pushArgs, stdout, stderr); err != nil {
		return err
	}
	remoteURL, _ := c.runCmd(ctx, r.GitRoot, []string{"git", "remote", "get-url", r.DefaultRemote})
	branch := shellQuote(r.Branch)
	baseRef := shellQuote(base.ref)
	// Configure remotes before the branch reset; the reset depends on the refs
	// and upstreams set by the preceding commands.
	return c.configureContainerRemotes(ctx, stdout, stderr, repoIdx, convertGitURLToHTTPS(remoteURL), base.useHost,
		"git switch -q -C "+branch+" "+baseRef,
		"git branch -q --set-upstream-to="+baseRef)
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

// ForkOpts configures a container fork operation.
type ForkOpts struct {
	// ExtraRepos are additional repos to map into the fork beyond the
	// source container's repos. Branch is the source branch to push from
	// the host; if empty, defaults to the repo's default upstream branch.
	// Fork generates a unique destination branch, same as for source repos.
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

// Fork snapshots a running container and creates a new one where each mapped
// repository is checked out on a new branch.
//
// The snapshot preserves the container's entire filesystem (installed
// packages, build artifacts, etc.) while giving each repo a fresh branch
// that diverges from the source container's working state.
//
// Branch naming: each repo (source and extra) gets its own unique destination
// branch derived from its source branch (e.g. "main" → "main-0").
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

	// Resolve extra repos: default Branch to the repo's default upstream branch.
	extraRepos := slices.Clone(opts.ExtraRepos)
	for i := range extraRepos {
		if extraRepos[i].Branch == "" {
			if err := extraRepos[i].resolveDefaults(ctx); err != nil {
				return nil, fmt.Errorf("resolving defaults for extra repo %s: %w", extraRepos[i].GitRoot, err)
			}
			extraRepos[i].Branch = extraRepos[i].DefaultBranch
		}
	}

	// Generate a unique destination branch per repo (source and extra),
	// derived from each repo's source branch.
	allSrc := append(slices.Clone(c.Repos), extraRepos...)
	forkRepos := slices.Clone(allSrc)
	existing, _ := c.List(ctx)
	for i, src := range allSrc {
		usedBranches := map[string]struct{}{}
		for _, ct := range existing {
			for _, r := range ct.Repos {
				if r.GitRoot == src.GitRoot {
					usedBranches[r.Branch] = struct{}{}
				}
			}
		}
		for n := 0; ; n++ {
			cand := fmt.Sprintf("%s-%d", src.Branch, n)
			if _, ok := usedBranches[cand]; ok {
				continue
			}
			if _, err := gitutil.RunGit(ctx, src.GitRoot, "rev-parse", "--verify", cand); err != nil {
				forkRepos[i].Branch = cand
				break
			}
		}
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
	labelCSV, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", `{{range $k, $v := .Config.Labels}}{{$k}} {{end}}`, c.Name})
	if err != nil {
		return nil, fmt.Errorf("inspecting labels: %w", err)
	}
	commitArgs := []string{c.Runtime, "commit"}
	for _, change := range forkSnapshotConfigChanges(labelCSV) {
		commitArgs = append(commitArgs, "--change", change)
	}
	commitArgs = append(commitArgs, c.Name, snapshotImage)
	if _, err := c.runCmd(ctx, "", commitArgs); err != nil {
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
		if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "fetch", "-q", c.Name, r.Branch}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from source container: %w", r.MountedPath, err)
		}
		fetchedRef := c.Name + "/" + r.Branch
		curr, _ := gitutil.CurrentBranch(ctx, r.GitRoot)
		newBranch := fork.Repos[i].Branch
		if curr == newBranch {
			if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "reset", "--hard", fetchedRef}, stdout, stderr); err != nil {
				return nil, fmt.Errorf("resetting branch %s: %w", newBranch, err)
			}
		} else {
			if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "branch", "-f", newBranch, fetchedRef}, stdout, stderr); err != nil {
				return nil, fmt.Errorf("creating branch %s: %w", newBranch, err)
			}
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

	// Inside the forked container: rename branches for source repos,
	// push extra repos as new.
	if !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- Setting up branches in forked container ...")
	}
	for i, r := range c.Repos {
		mp := shellQuote(r.MountedPath)
		oldBranch := shellQuote(r.Branch)
		newBranch := shellQuote(fork.Repos[i].Branch)

		hostBranch := shellQuote("host/" + fork.Repos[i].Branch)
		pushArgs := []string{"git", "push", "-q", "-f", fork.Name, "refs/heads/" + fork.Repos[i].Branch + ":refs/remotes/host/" + fork.Repos[i].Branch}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, pushArgs, stdout, stderr); err != nil {
			return nil, fmt.Errorf("pushing host branch for %s: %w", r.MountedPath, err)
		}
		renameCmd := "cd " + mp +
			" && " + hostRemoteSetupCommand +
			" && git branch -m " + oldBranch + " " + newBranch +
			" && git branch -q --set-upstream-to=" + hostBranch
		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, renameCmd), stdout, stderr); err != nil {
			return nil, fmt.Errorf("renaming branch for %s: %w", r.MountedPath, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{
			"git", "fetch", "-q", fork.Name, fork.Repos[i].Branch,
		}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from fork: %w", fork.Repos[i].Branch, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{
			"git", "branch", "-q", "--set-upstream-to", fork.Name + "/" + fork.Repos[i].Branch, fork.Repos[i].Branch,
		}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("setting upstream for %s: %w", fork.Repos[i].Branch, err)
		}
	}
	// Push extra repos into the container using source branch, set up
	// destination branch inside.
	nSrc := len(c.Repos)
	for i, src := range extraRepos {
		dst := forkRepos[nSrc+i]
		mp := shellQuote(src.MountedPath)
		dstBranch := shellQuote(dst.Branch)

		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, "git init -q "+mp), stdout, stderr); err != nil {
			return nil, fmt.Errorf("init extra repo %s in container: %w", src.MountedPath, err)
		}
		hostBranch := shellQuote("host/" + dst.Branch)
		pushArgs := []string{"git", "push", "-q", fork.Name, "refs/heads/" + src.Branch + ":refs/remotes/host/" + dst.Branch}
		if err := c.runCmdOut(ctx, src.GitRoot, pushArgs, stdout, stderr); err != nil {
			return nil, fmt.Errorf("push extra repo %s: %w", src.MountedPath, err)
		}
		setupCmd := "cd " + mp +
			" && " + hostRemoteSetupCommand +
			" && git branch -q --track " + dstBranch + " " + hostBranch +
			" && git switch -q " + dstBranch
		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, setupCmd), stdout, stderr); err != nil {
			return nil, fmt.Errorf("setting up extra repo %s: %w", src.MountedPath, err)
		}
	}

	fork.State = "running"
	return fork, nil
}

// forkSnapshotConfigChanges returns docker commit --change entries for a fork snapshot.
//
// It clears labels and runtime ENV inherited from the source container image so
// launchContainer can apply the fork's requested metadata and capabilities.
func forkSnapshotConfigChanges(labelCSV string) []string {
	labels := strings.Fields(labelCSV)
	changes := make([]string, 0, len(labels)+len(forkSnapshotEnvKeys))
	for _, key := range labels {
		changes = append(changes, "LABEL "+key+"=")
	}
	for _, key := range forkSnapshotEnvKeys {
		changes = append(changes, "ENV "+key+"=")
	}
	return changes
}

// ContainerStats holds runtime resource usage for a container.
type ContainerStats struct {
	// CPUPerc is the CPU usage as a percentage (e.g. 1.23).
	CPUPerc float64 `json:"cpu_perc"`
	// MemUsed is memory currently used in bytes.
	MemUsed uint64 `json:"mem_used"`
	// MemLimit is the memory limit in bytes.
	MemLimit uint64 `json:"mem_limit"`
	// MemPerc is the memory usage as a percentage (e.g. 2.0).
	MemPerc float64 `json:"mem_perc"`
	// PIDs is the number of running processes.
	PIDs int `json:"pids"`
	// NetRx is the total network bytes received.
	NetRx uint64 `json:"net_rx"`
	// NetTx is the total network bytes transmitted.
	NetTx uint64 `json:"net_tx"`
	// BlockRead is the total bytes read from block devices.
	BlockRead uint64 `json:"block_read"`
	// BlockWrite is the total bytes written to block devices.
	BlockWrite uint64 `json:"block_write"`
	// DiskUsed is the writable container layer size in bytes (-1 if unavailable).
	DiskUsed int64 `json:"disk_used"`
}

// Stats returns the current resource usage for the container, including CPU,
// memory, network I/O, block I/O, and writable-layer disk usage.
func (c *Container) Stats(ctx context.Context) (*ContainerStats, error) {
	out, err := c.runCmd(ctx, "", []string{
		c.Runtime, "stats", "--no-stream", "--no-trunc",
		"--format", "{{json .}}", c.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("container %s is not running", c.Name)
	}
	s, _, err := parseStatsLine(out)
	if err != nil {
		return nil, fmt.Errorf("parsing stats for %s: %w", c.Name, err)
	}
	s.DiskUsed, _ = c.DiskUsage(ctx)
	return s, nil
}

// DiskUsage returns the writable container layer size in bytes via
// docker inspect --size. Works for both running and stopped containers.
func (c *Container) DiskUsage(ctx context.Context) (int64, error) {
	out, err := c.runCmd(ctx, "", []string{
		c.Runtime, "inspect", "--size", "--format", "{{json .SizeRw}}", c.Name,
	})
	if err != nil {
		return -1, fmt.Errorf("inspecting container %s: %w", c.Name, err)
	}
	var sz int64
	if err := json.Unmarshal([]byte(out), &sz); err != nil {
		return -1, fmt.Errorf("parsing SizeRw: %w", err)
	}
	return sz, nil
}

// Status returns the Docker container state (e.g. "running", "exited", "").
// Returns empty string when the container does not exist.
func (c *Container) Status(ctx context.Context) string {
	out, err := c.runCmd(ctx, "", []string{
		c.Runtime, "inspect", "--format", "{{.State.Status}}", c.Name,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// GetHostPort returns the host port mapped to a container port (e.g.
// "5901/tcp"). Returns 0 if the port is not mapped.
func (c *Container) GetHostPort(ctx context.Context, containerPort string) (int32, error) {
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name}); err != nil {
		return 0, fmt.Errorf("container %s is not running", c.Name)
	}
	return c.getHostPort(ctx, c.Name, containerPort)
}

// Inspect returns detailed observed runtime configuration for the container.
func (c *Container) Inspect(ctx context.Context) (*InspectInfo, error) {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name})
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", c.Name, err)
	}
	info, err := parseInspectInfo(c.Runtime, c.Name, []byte(out))
	if err != nil {
		return nil, fmt.Errorf("parsing container %s: %w", c.Name, err)
	}
	c.fillInspectOSArch(ctx, info)
	return info, nil
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
	out, err := c.runCmd(ctx, "", []string{
		c.Runtime, "inspect", "--format",
		`{{index .Config.Labels "md.sudo-password"}}`, c.Name,
	})
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
	statusJSON, err := c.runCmd(ctx, "", []string{c.Runtime, "exec", c.Name, "tailscale", "status", "--json"})
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
// for Repos[repoIdx] into the container so agents can diff against it. It also
// refreshes matching remote-tracking refs.
func (c *Container) SyncDefaultBranch(ctx context.Context, repoIdx int) error {
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	refspecs, err := c.Repos[repoIdx].containerSyncRefspecs(ctx)
	if err != nil {
		return err
	}
	if err := c.pushContainerRefs(ctx, &c.Repos[repoIdx], refspecs); err != nil {
		return fmt.Errorf("sync refs %q: %w", refspecs, err)
	}
	return nil
}

func (r *Repo) containerSyncRefspecs(ctx context.Context) ([]string, error) {
	// These refspecs refresh the container's copy of the default branch and the
	// tracked branch, when distinct. Callers append them to the push that already
	// transfers the task branch.
	if err := r.resolveDefaults(ctx); err != nil {
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
	// Containers can outlive the branch recorded at launch. If that branch was
	// deleted locally and remotely, leave the existing container refs in place.
	branch, src, ok, err := r.trackedBranchPushSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync tracked branch for %q: %w", r.Branch, err)
	}
	if ok && branch != r.DefaultBranch {
		refspecs = append(refspecs, src+":"+remoteTrackingRef(r.DefaultRemote, branch))
	}
	return refspecs, nil
}

// revokeSudo removes sudo access from a forked container.
//
// A fork created from a sudo-enabled snapshot can inherit /etc/group and the
// user's password hash. Revocation makes an explicit non-sudo fork match its
// requested capabilities before the SSH readiness probe runs.
func (c *Container) revokeSudo(ctx context.Context) error {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "exec", "--user", "0:0", c.Name, "bash", "-lc", revokeSudoCommand})
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
	raw, err := inspectDocument(data)
	if err != nil {
		return err
	}
	c.Name = strings.TrimPrefix(raw.Name, "/")
	c.State = raw.State.Status
	c.Labels = maps.Clone(raw.Config.Labels)
	c.SSHPort = hostPort(raw.NetworkSettings.Ports, "22/tcp")
	c.VNCPort = hostPort(raw.NetworkSettings.Ports, "5901/tcp")
	if t, err := parseCreatedAt(raw.Created); err == nil {
		c.CreatedAt = t
	}
	for k, v := range raw.Config.Labels {
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
	// container ref when the task branch is also the default/tracked branch.
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

type containerBranchBase struct {
	source      string
	ref         string
	useHost     bool
	destination string
}

func (r *Repo) resolveContainerBranchBase(ctx context.Context) (containerBranchBase, error) {
	localRef := "refs/heads/" + r.Branch
	localCommit, err := gitutil.RevParse(ctx, r.GitRoot, localRef)
	if err != nil {
		return containerBranchBase{}, err
	}
	remote, branch, ok, err := r.branchUpstream(ctx)
	if err != nil {
		return containerBranchBase{}, err
	}
	if ok && remote == r.DefaultRemote {
		remoteRef := remoteTrackingRef(remote, branch)
		if remoteCommit, err := gitutil.RevParse(ctx, r.GitRoot, remoteRef); err == nil && remoteCommit == localCommit {
			return containerBranchBase{source: remoteRef, ref: remote + "/" + branch, destination: remoteRef}, nil
		}
	}
	remoteRef := remoteTrackingRef(r.DefaultRemote, r.Branch)
	if remoteCommit, err := gitutil.RevParse(ctx, r.GitRoot, remoteRef); err == nil && remoteCommit == localCommit {
		return containerBranchBase{source: remoteRef, ref: r.DefaultRemote + "/" + r.Branch, destination: remoteRef}, nil
	}
	return containerBranchBase{source: localRef, ref: "host/" + r.Branch, useHost: true, destination: "refs/remotes/host/" + r.Branch}, nil
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

func (r *Repo) trackedBranchPushSource(ctx context.Context) (branch, src string, ok bool, err error) {
	remote, branch, ok, err := r.branchUpstream(ctx)
	if err != nil || !ok || remote != r.DefaultRemote {
		return "", "", false, err
	}
	src = remoteTrackingRef(remote, branch)
	if _, err := gitutil.RevParse(ctx, r.GitRoot, src); err != nil {
		return "", "", false, err
	}
	return branch, src, true, nil
}

func (r *Repo) branchUpstream(ctx context.Context) (remote, branch string, ok bool, err error) {
	out, err := gitutil.RunGit(ctx, r.GitRoot, "for-each-ref", "--format=%(upstream:remotename)%00%(upstream:remoteref)", "refs/heads/"+r.Branch)
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
		return "", "", false, fmt.Errorf("invalid upstream for branch %q: %q", r.Branch, out)
	}
	branch, ok = strings.CutPrefix(remoteRef, "refs/heads/")
	if !ok || branch == "" {
		return "", "", false, fmt.Errorf("invalid upstream ref for branch %q: %q", r.Branch, remoteRef)
	}
	return remote, branch, true, nil
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
	statusJSON, statusErr := c.runCmd(ctx, "", []string{c.Runtime, "exec", c.Name, "tailscale", "status", "--json"})
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

func (c *Container) readContainerFile(ctx context.Context, containerPath string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "md-container-file-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	dst := filepath.Join(tmpDir, "file")
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "cp", c.Name + ":" + containerPath, dst}); err != nil {
		return "", err
	}
	data, err := os.ReadFile(dst) // #nosec G304 -- dst is a private temp file populated by docker cp.
	if err != nil {
		return "", err
	}
	return string(data), nil
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
	_, containerErr := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name})
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
			return fmt.Errorf("no container running for branch '%s'.\nStart a container with: md start", c.Repos[0].Branch)
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
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name})
	if err != nil {
		return err
	}
	return c.fillFromInspect(ctx, []byte(out))
}

// ensureImage checks whether the user image needs rebuilding and, if so,
// builds it. Returns the computed image name (keyed by base image and active
// caches). The build is serialized via Client.buildMu.
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
		return imageName, nil
	}
	if err := c.buildSpecializedImage(ctx, stdout, stderr, imageName, baseImage, platform, caches, agentContainerPaths(), quiet); err != nil {
		return "", err
	}
	c.invalidateImageBuildCache()
	return imageName, nil
}

// pushSubmodules transfers submodule bare repos from hostGitRoot into the
// container at containerRepoPath and initializes them at all nesting depths
// without requiring network access. containerRepoPath is the absolute path
// inside the container (e.g. /home/user/src/myrepo).
//
// Returns nil when no submodules are declared or when submodules are declared
// but not yet cloned on the host (uninitialized).
func (c *Container) pushSubmodules(ctx context.Context, stdout, stderr io.Writer, containerRepoPath, hostGitRoot string, quiet bool) error {
	subs, err := gitutil.ListSubmodules(ctx, hostGitRoot)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	moduleDirs, err := gitutil.FindModuleDirs(hostGitRoot)
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

// byteUnits maps suffixes used by docker/podman stats to multipliers.
var byteUnits = []struct {
	suffix string
	mult   uint64
}{
	{"KiB", 1 << 10},
	{"MiB", 1 << 20},
	{"GiB", 1 << 30},
	{"TiB", 1 << 40},
	{"kB", 1000},
	{"MB", 1000 * 1000},
	{"GB", 1000 * 1000 * 1000},
	{"TB", 1000 * 1000 * 1000 * 1000},
	{"B", 1},
}

// launchContainer starts the Docker container, queries mapped ports, writes
// SSH config, and sets up host-side git remotes. It does NOT wait for SSH.
// Port and creation-time results are stored directly on c (launchSSHPort,
// launchVNCPort, CreatedAt) so that connectContainer can complete startup.
func (c *Container) launchContainer(ctx context.Context, stdout, stderr io.Writer, opts *StartOpts, imageName string) error {
	if len(c.Repos) > 1000 {
		return fmt.Errorf("too many repositories: %d (max 1000)", len(c.Repos))
	}
	if opts.Sudo && isRootlessPodman(c.Runtime) {
		return errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	p := Platform(opts.Platform).Resolve()
	if err := p.Validate(); err != nil {
		return err
	}
	platform := p.String()
	dockerArgs := []string{
		c.Runtime, "run", "-d",
		"--platform", platform,
		"--name", c.Name,
		"--hostname", c.Name,
		"-p", "127.0.0.1::22",
		// Localtime: mount the host timezone file. Docker Desktop on Windows/macOS provides
		// a virtual /etc/localtime inside the VM, so the flag is universal.
		"-v", "/etc/localtime:/etc/localtime:ro",
	}
	if opts.MaxCPUs > 0 {
		dockerArgs = append(dockerArgs, "--cpus", strconv.Itoa(opts.MaxCPUs))
	}

	if opts.Display {
		dockerArgs = append(dockerArgs, "-p", "127.0.0.1::5901", "-e", "MD_DISPLAY=1")
	}

	if kvmAvailable() {
		dockerArgs = append(dockerArgs, "--device=/dev/kvm")
	}
	// Sandbox capabilities.
	// - SYS_PTRACE: needed for strace/debuggers. Scoped to the container's
	//   PID namespace — cannot attach to host processes.
	// - seccomp=unconfined: disables the syscall allowlist so strace, bpf,
	//   and Chrome's sandbox work. Does NOT grant capabilities — the
	//   capability set still limits what the process can do.
	dockerArgs = append(dockerArgs,
		"--cap-add=SYS_PTRACE",
		"--security-opt", "seccomp=unconfined")
	// - apparmor=unconfined: disables AppArmor's mandatory-access-control
	//   profile so Chrome can create namespaces and sandboxed processes can
	//   access /proc. Docker-only; podman uses SELinux and passing this
	//   option can hang on kernel security filesystem access.
	if c.Runtime != "podman" {
		dockerArgs = append(dockerArgs, "--security-opt", "apparmor=unconfined")
	}

	// Rootless podman: --userns=keep-id maps host UID to same UID inside the
	// container so bind-mounted configs are writable. --user 0:0 keeps
	// start.sh running as root for privileged setup (groupmod, sshd, dbus).
	// Rootless Docker is handled inside start.sh via /proc/self/uid_map
	// detection since Docker lacks --userns=keep-id.
	if isRootlessPodman(c.Runtime) {
		dockerArgs = append(dockerArgs, "--userns=keep-id", "--user", "0:0")
	}

	// NET_ADMIN and NET_RAW are always granted:
	// - tcpdump uses AF_PACKET sockets which require NET_RAW.
	// - Tailscale manipulates the network interface (route table changes)
	//   which requires NET_ADMIN.
	// Both are scoped to the container's network namespace.
	dockerArgs = append(dockerArgs,
		"--cap-add=NET_ADMIN", "--cap-add=NET_RAW")

	// Pass through the host TUN device when Tailscale or rootless Podman
	// (via -sudo) need to create network interfaces.
	if opts.Tailscale || opts.Sudo {
		dockerArgs = append(dockerArgs, "--device=/dev/net/tun:/dev/net/tun")
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
		dockerArgs = append(dockerArgs,
			"-e", "MD_TAILSCALE=1")
		if opts.TailscaleAuthKey != "" {
			dockerArgs = append(dockerArgs, "-e", "TAILSCALE_AUTHKEY="+opts.TailscaleAuthKey)
		}
		if c.tailscaleEphemeral {
			dockerArgs = append(dockerArgs, "-e", "MD_TAILSCALE_EPHEMERAL=1")
		}
		if opts.resetTailscale {
			dockerArgs = append(dockerArgs, "-e", "MD_TAILSCALE_RESET=1")
		}
	}

	// USB passthrough (Linux only; Docker Desktop on macOS/Windows runs in a
	// VM that cannot access host USB devices). Use a bind mount + cgroup
	// rule so that devices plugged in after container start are visible.
	if opts.USB {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("--usb requires Linux; Docker Desktop on %s cannot pass through host USB devices", runtime.GOOS)
		}
		dockerArgs = append(dockerArgs,
			"-v", "/dev/bus/usb:/dev/bus/usb",
			"--device-cgroup-rule=c 189:* rwm")
	}

	home := c.Home
	for _, m := range opts.Mounts {
		arg, err := m.dockerArg(home)
		if err != nil {
			return err
		}
		dockerArgs = append(dockerArgs, "-v", arg)
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
		dockerArgs = append(dockerArgs,
			"--label", "md.sudo=1",
			"--label", "md.sudo-password="+sudoPassword,
			"-e", "MD_SUDO_PASSWORD="+sudoPassword,
			"--cap-add=SYS_ADMIN",
			"--device=/dev/fuse")
	}
	if reposJSON, err := json.Marshal(c.Repos); err == nil {
		// Base64-encode so commas in JSON don't corrupt the comma-separated
		// label parsing in unmarshalContainer.
		dockerArgs = append(dockerArgs, "--label", "md.repos="+base64.StdEncoding.EncodeToString(reposJSON))
	}
	if opts.Display {
		dockerArgs = append(dockerArgs, "--label", "md.display=1")
	}
	if opts.Tailscale {
		dockerArgs = append(dockerArgs, "--label", "md.tailscale=1")
		if c.tailscaleEphemeral {
			dockerArgs = append(dockerArgs, "--label", "md.tailscale_ephemeral=1")
		}
	}
	if opts.USB {
		dockerArgs = append(dockerArgs, "--label", "md.usb=1")
	}
	for _, l := range opts.Labels {
		dockerArgs = append(dockerArgs, "--label", l)
	}
	dockerArgs = append(dockerArgs, opts.ExtraRunArgs...)
	dockerArgs = append(dockerArgs, imageName)

	if opts.Quiet {
		if _, err := c.runCmd(ctx, "", dockerArgs); err != nil {
			return cmdErrWithStderr("starting container", err)
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "- Starting container %s ... ", c.Name)
		if err := c.runCmdOut(ctx, "", dockerArgs, stdout, stderr); err != nil {
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
	out, err := c.runCmd(readCtx, "", []string{c.Runtime, "exec", c.Name, "sh", "-c", script})
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
// The task branch and default branch are pushed in parallel to reduce latency.
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
	if len(c.Repos) > 0 {
		if !opts.Quiet {
			_, _ = fmt.Fprintln(stdout, "- git clone into container ...")
		}
		eg, egCtx := errgroup.WithContext(ctx)
		for repoIdx := range c.Repos {
			eg.Go(func() error {
				mp := shellQuote(c.Repos[repoIdx].MountedPath)
				rBranch := shellQuote(c.Repos[repoIdx].Branch)

				if err := c.runCmdOut(egCtx, "", c.SSHCommand(nil, "git init -q "+mp), stdout, stderr); err != nil {
					return fmt.Errorf("init repo %s in container: %w", c.Repos[repoIdx].MountedPath, err)
				}

				if err := c.Repos[repoIdx].resolveDefaults(egCtx); err != nil {
					return fmt.Errorf("resolve defaults for %s: %w", c.Repos[repoIdx].MountedPath, err)
				}
				base, err := c.Repos[repoIdx].resolveContainerBranchBase(egCtx)
				if err != nil {
					return fmt.Errorf("resolve branch base for %s: %w", c.Repos[repoIdx].MountedPath, err)
				}
				// Seed the task branch and the refs used as diff bases in one push. The
				// checkout below can then set the branch upstream against refs that already
				// exist in the container.
				syncRefspecs, err := c.Repos[repoIdx].containerSyncRefspecs(egCtx)
				if err != nil {
					return err
				}
				refspecs := appendUniqueRefspecs([]string{base.source + ":" + base.destination}, syncRefspecs...)
				pushArgs := append([]string{"git", "push", "-q", c.Name}, refspecs...)
				if err := c.runCmdOut(egCtx, c.Repos[repoIdx].GitRoot, pushArgs, stdout, stderr); err != nil {
					return fmt.Errorf("push repo %s: %w", c.Repos[repoIdx].MountedPath, err)
				}
				remoteURL, _ := c.runCmd(egCtx, c.Repos[repoIdx].GitRoot, []string{"git", "remote", "get-url", c.Repos[repoIdx].DefaultRemote})
				httpsURL := convertGitURLToHTTPS(remoteURL)
				if !opts.Quiet && httpsURL != "" {
					_, _ = fmt.Fprintf(stdout, "- Set %s %s to %s\n", c.Repos[repoIdx].MountedPath, c.Repos[repoIdx].DefaultRemote, httpsURL)
				}
				baseRef := shellQuote(base.ref)
				if err := c.configureContainerRemotes(egCtx, stdout, stderr, repoIdx, httpsURL, base.useHost,
					"git checkout -q -B "+rBranch+" "+baseRef,
					"git branch -q --set-upstream-to="+baseRef); err != nil {
					return err
				}

				if err := c.pushSubmodules(egCtx, stdout, stderr, c.Repos[repoIdx].MountedPath, c.Repos[repoIdx].GitRoot, opts.Quiet); err != nil {
					return fmt.Errorf("push submodules for %s: %w", c.Repos[repoIdx].MountedPath, err)
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

// parseByteSize parses a size string like "150MiB" or "7.5GiB" into bytes.
func parseByteSize(s string) (uint64, error) {
	for _, u := range byteUnits {
		if numStr, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
			if err != nil {
				return 0, fmt.Errorf("parsing %q: %w", s, err)
			}
			return uint64(f * float64(u.mult)), nil
		}
	}
	return 0, fmt.Errorf("unknown unit in %q", s)
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

func (c *Container) fillInspectOSArch(ctx context.Context, info *InspectInfo) {
	if info.OS != "" && info.Architecture != "" {
		return
	}
	fallbackOS := info.OS
	if fallbackOS == "" {
		fallbackOS = cleanInspectValue(info.Platform)
	}
	if observedOS, observedArchitecture, ok := c.inspectTargetOSArch(ctx, []string{"inspect", c.Name, "--format", "{{.Os}}/{{.Architecture}}"}, fallbackOS); ok {
		info.OS = observedOS
		info.Architecture = observedArchitecture
		return
	}
	for _, image := range []string{info.ImageID, info.ImageRef} {
		if image == "" {
			continue
		}
		observedOS, observedArchitecture, ok := c.inspectTargetOSArch(ctx, []string{"image", "inspect", image, "--format", "{{.Os}}/{{.Architecture}}"}, fallbackOS)
		if ok {
			info.OS = observedOS
			info.Architecture = observedArchitecture
			return
		}
	}
}

func (c *Container) inspectTargetOSArch(ctx context.Context, args []string, fallbackOS string) (osName, architecture string, ok bool) {
	if c.Runtime == "" {
		return "", "", false
	}
	out, err := c.runCmd(ctx, "", append([]string{c.Runtime}, args...))
	if err != nil {
		return "", "", false
	}
	osName, architecture, ok = splitOSArch(out, fallbackOS)
	if !ok || fallbackOS != "" && osName != fallbackOS {
		return "", "", false
	}
	return osName, architecture, true
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

func hostPort(ports map[string][]inspectPortBindingJSON, containerPort string) int32 {
	bindings := ports[containerPort]
	if len(bindings) == 0 {
		return 0
	}
	port, err := strconv.ParseInt(bindings[0].HostPort, 10, 32)
	if err != nil {
		return 0
	}
	return int32(port)
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

// parsePercent parses a percentage string like "1.23%" into 1.23.
// Returns 0 for "N/A" (unavailable cgroup metrics).
func parsePercent(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "N/A" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "%")
	return strconv.ParseFloat(s, 64)
}

// parseMemUsage parses "150MiB / 7.5GiB" into (used, limit) in bytes.
// Returns (0, 0) for "N/A / N/A" (unavailable cgroup metrics).
func parseMemUsage(s string) (used, limit uint64, err error) {
	if strings.TrimSpace(s) == "N/A / N/A" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected 'used / limit', got %q", s)
	}
	used, err = parseByteSize(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	limit, err = parseByteSize(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return used, limit, nil
}

// parseIOPair parses "1.23kB / 456B" (docker NetIO / BlockIO) into two byte counts.
// Returns (0, 0) for "N/A / N/A" (unavailable cgroup metrics).
func parseIOPair(s string) (a, b uint64, err error) {
	if strings.TrimSpace(s) == "N/A / N/A" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected 'a / b', got %q", s)
	}
	a, err = parseByteSize(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	b, err = parseByteSize(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return a, b, nil
}
