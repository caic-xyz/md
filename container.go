// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

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
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
	"golang.org/x/term"
)

// DefaultBaseImage is the base image used when none is specified.
const DefaultBaseImage = "ghcr.io/caic-xyz/md-user"

// Repo describes a git repository to push into a container.
type Repo struct {
	// GitRoot is the absolute path to the git repository root on the host.
	GitRoot string `json:"git_root"`
	// Branch is the git branch to push into the container.
	Branch string `json:"branch"`
	// MountedPath is the absolute destination path inside the
	// container, e.g. "/home/user/src/github/caic". When empty,
	// populateMountPath fills it from filepath.Base(GitRoot).
	// Callers may set it explicitly for disambiguation.
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
	// Labels are additional Docker labels (key=value) applied to the container.
	Labels []string
	// Quiet suppresses informational output during startup.
	Quiet bool
	// AgentPaths specifies which agent config directories to mount. Pass one
	// entry per harness using values from [HarnessMounts]. Always-mounted
	// directories (~/.config/agents, ~/.config/md) are added automatically.
	// Nil or empty mounts only those shared directories.
	AgentPaths []AgentPaths
	// ExtraEnv holds additional KEY=VALUE pairs to inject into the container's
	// ~/.env at runtime. Each entry is appended verbatim, so values may
	// contain spaces but must not contain newlines.
	ExtraEnv []string
	// MaxCPUs limits the number of CPU cores the container may use.
	// Passed as --cpus to docker/podman. Zero means no limit.
	// Use [DefaultMaxCPUs] for a sensible default.
	MaxCPUs int
	// ExtraRunArgs are additional arguments passed verbatim to the
	// container runtime's "run" command. Not portable across runtimes.
	ExtraRunArgs []string
}

// StartResult contains Tailscale information from Connect. Port information
// is available on Container directly (SSHPort, VNCPort) after Launch returns.
type StartResult struct {
	// TailscaleFQDN is the Tailscale FQDN assigned to the container, if any.
	TailscaleFQDN string
	// TailscaleAuthURL is the Tailscale auth URL when no pre-auth key was provided.
	TailscaleAuthURL string
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

// Validate normalizes the repo and returns an error for invalid values.
func (r *Repo) Validate() error {
	r.populateMountPath()
	if strings.HasPrefix(r.MountedPath, "~/") {
		r.MountedPath = "/home/user/" + r.MountedPath[2:]
	}
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

// validateRepoNames checks that all repos have unique mount paths.
//
// Two repos with the same MountedPath would share a directory and
// overwrite each other. Callers must set MountedPath to disambiguate repos
// with the same basename from different parent directories.
func validateRepoNames(repos []Repo) error {
	seen := make(map[string]int, len(repos))
	for i, r := range repos {
		n := r.MountedPath
		if j, dup := seen[n]; dup {
			return fmt.Errorf("repos[%d] and repos[%d] both mount as %q; set MountedPath to disambiguate", j, i, n)
		}
		seen[n] = i
	}
	return nil
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
	if err := c.prepare(opts.AgentPaths); err != nil {
		return err
	}
	// Validate unique mount paths within this container. Two repos with
	// the same MountedPath would share a directory and overwrite each
	// other's data. Set MountedPath explicitly to disambiguate.
	if err := validateRepoNames(c.Repos); err != nil {
		return err
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

	// Generate Tailscale auth key if needed.
	if opts.Tailscale && opts.TailscaleAuthKey == "" {
		key, err := generateTailscaleAuthKey(ctx, c.TailscaleAPIKey)
		if err != nil {
			if !opts.Quiet {
				_, _ = fmt.Fprintf(stdout, "- Could not generate Tailscale auth key (%v), will use browser auth\n", err)
			}
		} else {
			opts.TailscaleAuthKey = key
			c.tailscaleEphemeral = true
		}
	}

	baseImage := opts.BaseImage
	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	imageName, err := c.ensureImage(ctx, stdout, stderr, baseImage, opts.Caches, opts.Quiet)
	if err != nil {
		return err
	}
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

// Run starts a temporary container, runs a command, then cleans up.
// baseImage is the full Docker image reference; if empty, DefaultBaseImage is
// used. caches lists host directories to COPY into the image (same semantics
// as StartOpts.Caches); nil means no caches. extraEnv holds KEY=VALUE pairs
// injected into the container's ~/.env (see StartOpts.ExtraEnv).
func (c *Container) Run(ctx context.Context, stdout, stderr io.Writer, baseImage string, command []string, caches []CacheMount, extraEnv []string, maxCPUs int, extraRunArgs []string) (_ int, retErr error) {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	var tmpRepos []Repo
	var tmpName string
	if len(c.Repos) > 0 {
		tmpRepos = c.Repos[:1]
		tmpName = fmt.Sprintf("md-%s-run-%x", SanitizeDockerName(filepath.Base(c.Repos[0].MountedPath)), buf)
	} else {
		tmpName = fmt.Sprintf("md-run-%x", buf)
	}
	tmp := &Container{
		Client: c.Client,
		Repos:  tmpRepos,
		Name:   tmpName,
	}

	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	imageName, err := c.ensureImage(ctx, stdout, stderr, baseImage, caches, true)
	if err != nil {
		return 1, err
	}
	opts := StartOpts{Quiet: true, ExtraEnv: extraEnv, AgentPaths: slices.Collect(maps.Values(HarnessMounts)), MaxCPUs: maxCPUs, ExtraRunArgs: extraRunArgs}
	if err := tmp.launchContainer(ctx, stdout, stderr, &opts, imageName); err != nil {
		tmp.cleanup(ctx)
		return 1, err
	}
	if _, err := tmp.provisionContainer(ctx, stdout, stderr, &opts); err != nil {
		tmp.cleanup(ctx)
		return 1, err
	}

	cmdStr := strings.Join(command, " ")
	var sshCmd string
	if len(c.Repos) > 0 {
		sshCmd = "cd " + shellQuote(c.Repos[0].MountedPath) + " && " + cmdStr
	} else {
		sshCmd = cmdStr
	}
	err = c.runCmdOut(ctx, "", tmp.SSHCommand(nil, sshCmd), stdout, stderr)
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	tmp.cleanup(ctx)
	return exitCode, nil
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

	// Query the new SSH port (port mapping changes on restart).
	port, err := c.getHostPort(ctx, c.Name, "22/tcp")
	if err != nil {
		return fmt.Errorf("getting SSH port after revive: %w", err)
	}
	c.SSHPort = port

	if c.Display {
		vncPort, _ := c.getHostPort(ctx, c.Name, "5901/tcp")
		c.VNCPort = vncPort
	}

	// Rewrite SSH config with the new port. The known_hosts file also
	// needs rewriting because entries are keyed by [127.0.0.1]:port.
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	removeSSHConfig(ctx, sshConfigDir, c.Name)
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
	deadline := time.Now().Add(30 * time.Second)
	if err := waitForTCP(ctx, addr, deadline); err != nil {
		return fmt.Errorf("waiting for SSH port on %s: %w", c.Name, err)
	}
	if err := c.waitForSSH(ctx, deadline); err != nil {
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
	cleanupControlSocket(ctx, c.Name)
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

	// Clean up non-ephemeral Tailscale node.
	if containerExists {
		if !c.Tailscale {
			tsLabel, _ := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", `{{index .Config.Labels "md.tailscale"}}`, c.Name})
			c.Tailscale = tsLabel == "1"
		}
		if c.Tailscale {
			ephLabel, _ := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", `{{index .Config.Labels "md.tailscale_ephemeral"}}`, c.Name})
			if ephLabel != "1" {
				statusJSON, err := c.runCmd(ctx, "", []string{c.Runtime, "exec", c.Name, "tailscale", "status", "--json"})
				if err == nil {
					var status tailscaleStatus
					if json.Unmarshal([]byte(statusJSON), &status) == nil && status.Self.ID != "" {
						_, _ = fmt.Fprintln(stdout, "- Removing Tailscale node from tailnet...")
						if err := deleteTailscaleDevice(ctx, c.TailscaleAPIKey, status.Self.ID); err != nil {
							slog.WarnContext(ctx, "md", "msg", "failed to remove Tailscale device", "err", err)
						}
					}
				}
			}
		}
	}

	_ = os.Remove(sshConf)
	_ = os.Remove(sshKnown)

	var retErr error
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
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return "", err
	}
	r := c.Repos[repoIdx]
	mp := shellQuote(r.MountedPath)
	branch := shellQuote(r.Branch)
	// Commit any pending changes in the container.
	_, _ = c.runCmd(ctx, "", c.SSHCommand(nil, "cd "+mp+" && git add . && (git diff --quiet HEAD -- . || git commit -q -m 'Backup before push')"))
	// Refuse if there are pending local changes on the branch being pushed.
	currentBranch, _ := gitutil.RunGit(ctx, r.GitRoot, "branch", "--show-current")
	if currentBranch == r.Branch {
		if _, err := gitutil.RunGit(ctx, r.GitRoot, "diff", "--quiet", "--exit-code"); err != nil {
			return "", errors.New("there are pending changes locally. Please commit or stash them before pushing")
		}
	}
	// Save a backup branch of the current container state.
	containerCommit, _ := c.runCmd(ctx, "", c.SSHCommand(nil, "cd "+mp+" && git rev-parse HEAD"))
	backupBranch := "backup-" + time.Now().Format("20060102-150405")
	_, _ = c.runCmd(ctx, "", c.SSHCommand(nil, "cd "+mp+" && git branch -f "+backupBranch+" "+shellQuote(containerCommit)))
	if err := c.runCmdOut(ctx, r.GitRoot, []string{"git", "push", "-q", "-f", "--tags", c.Name, r.Branch + ":base"}, stdout, stderr); err != nil {
		return "", err
	}
	if err := c.runCmdOut(ctx, "", c.SSHCommand(nil, "cd "+mp+" && git switch -q -C "+branch+" base && git branch --set-upstream-to=base"), stdout, stderr); err != nil {
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
	r := c.Repos[repoIdx]
	mp := shellQuote(r.MountedPath)
	if err := c.SyncDefaultBranch(ctx, repoIdx); err != nil {
		return err
	}
	// Check if there are uncommitted changes in the container.
	if _, err := c.runCmd(ctx, "", c.SSHCommand(nil, "cd "+mp+" && git add . && git diff --quiet HEAD -- .")); err != nil {
		commitMsg := "Pull from md"
		if p != nil {
			metadata := c.gatherGitMetadata(ctx, r.MountedPath)
			diff := c.gatherGitDiff(ctx, r.MountedPath)
			if msg, err := gitutil.GenerateCommitMsg(ctx, p, metadata, diff, nil); err != nil {
				slog.WarnContext(ctx, "md", "msg", "failed to generate commit message", "err", err)
			} else if msg != "" {
				commitMsg = msg
			}
		}
		gitUserName, _ := gitutil.RunGit(ctx, r.GitRoot, "config", "user.name")
		gitUserEmail, _ := gitutil.RunGit(ctx, r.GitRoot, "config", "user.email")
		if gitUserName == "" {
			gitUserName = "md"
		}
		if gitUserEmail == "" {
			gitUserEmail = "md@localhost"
		}
		gitAuthor := shellQuote(gitUserName + " <" + gitUserEmail + ">")
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
	r := c.Repos[repoIdx]
	remoteRef := c.Name + "/" + r.Branch
	currentBranch, _ := gitutil.RunGit(ctx, r.GitRoot, "branch", "--show-current")
	if currentBranch == r.Branch {
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
	return c.runCmdOut(ctx, r.GitRoot, []string{"git", "push", "-q", "-f", c.Name, r.Branch + ":base"}, stdout, stderr)
}

// Diff writes the diff between base and current for Repos[repoIdx] to stdout/stderr.
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
	quotedArgs := make([]string, len(extraArgs))
	for i, a := range extraArgs {
		quotedArgs[i] = shellQuote(a)
	}
	repo := c.Repos[repoIdx]
	mp := shellQuote(repo.MountedPath)
	opts := []string{"-q"}
	isTTY := false
	if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		opts = append(opts, "-t")
		isTTY = true
	}
	sshArgs := c.SSHCommand(opts, "cd "+mp+" && git add . && git diff base "+strings.Join(quotedArgs, " ")+" -- .")
	cmd := exec.CommandContext(ctx, sshArgs[0]) //nolint:gosec // args are from trusted SSH config
	cmd.Env = append(os.Environ(), c.env...)
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

// ForkOpts configures a container fork operation.
type ForkOpts struct {
	// ExtraRepos are additional repos to map into the fork beyond the
	// source container's repos. Branch is the source branch to push from
	// the host; if empty, defaults to the repo's default upstream branch.
	// Fork generates a unique destination branch, same as for source repos.
	ExtraRepos []Repo
	// Display enables X11/VNC virtual display on the forked container.
	// When false, inherits the source container's setting.
	Display bool
	// Tailscale enables Tailscale networking on the forked container.
	// When false, inherits the source container's setting.
	Tailscale bool
	// USB enables USB device passthrough on the forked container.
	// When false, inherits the source container's setting.
	USB bool
	// Sudo enables root access via sudo on the forked container.
	// When false, inherits the source container's setting.
	Sudo bool
	// Labels are additional Docker labels (key=value) applied to the forked container.
	Labels []string
	// Quiet suppresses informational output.
	Quiet bool
	// AgentPaths specifies which agent config directories to mount.
	AgentPaths []AgentPaths
	// ExtraEnv holds additional KEY=VALUE pairs to inject into the container's
	// ~/.env at runtime.
	ExtraEnv []string
	// MaxCPUs limits the number of CPU cores the forked container may use.
	// Passed as --cpus to docker/podman. Zero means no limit.
	// Use [DefaultMaxCPUs] for a sensible default.
	MaxCPUs int
	// ExtraRunArgs are additional arguments passed verbatim to the
	// container runtime's "run" command. Not portable across runtimes.
	ExtraRunArgs []string
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
	for key := range strings.FieldsSeq(labelCSV) {
		commitArgs = append(commitArgs, "--change", "LABEL "+key+"=")
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
	startOpts := &StartOpts{
		Quiet:        opts.Quiet,
		Labels:       opts.Labels,
		AgentPaths:   opts.AgentPaths,
		ExtraEnv:     opts.ExtraEnv,
		Display:      c.Display || opts.Display,
		Tailscale:    c.Tailscale || opts.Tailscale,
		USB:          c.USB || opts.USB,
		Sudo:         c.Sudo || opts.Sudo,
		MaxCPUs:      opts.MaxCPUs,
		ExtraRunArgs: opts.ExtraRunArgs,
	}
	if err := c.prepare(startOpts.AgentPaths); err != nil {
		return nil, err
	}
	if err := fork.launchContainer(ctx, stdout, stderr, startOpts, snapshotImage); err != nil {
		return nil, err
	}
	fork.Display = startOpts.Display
	fork.USB = startOpts.USB
	fork.Sudo = startOpts.Sudo

	// Wait for SSH and set up repos.
	addr := fmt.Sprintf("localhost:%d", fork.SSHPort)
	deadline := time.Now().Add(30 * time.Second)
	if err := waitForTCP(ctx, addr, deadline); err != nil {
		return nil, fmt.Errorf("waiting for SSH on forked container: %w", err)
	}

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
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		for _, kv := range startOpts.ExtraEnv {
			envContent = append(envContent, []byte(kv+"\n")...)
		}
	}
	sshEnvArgs := fork.SSHCommand(nil, "cat > /home/user/.env")
	for {
		cmd := exec.CommandContext(ctx, sshEnvArgs[0], sshEnvArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Env = append(os.Environ(), fork.env...)
		cmd.Stdin = bytes.NewReader(envContent)
		out, err := cmd.CombinedOutput()
		if err == nil {
			break
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 || time.Now().After(deadline) {
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

		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{
			"git", "push", "-q", "-f", fork.Name,
			fork.Repos[i].Branch + ":refs/heads/base",
		}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("pushing base for %s: %w", r.MountedPath, err)
		}
		renameCmd := "cd " + mp +
			" && git branch -m " + oldBranch + " " + newBranch +
			" && git branch --set-upstream-to=base"
		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, renameCmd), stdout, stderr); err != nil {
			return nil, fmt.Errorf("renaming branch for %s: %w", r.MountedPath, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{
			"git", "fetch", "-q", fork.Name, fork.Repos[i].Branch,
		}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("fetching %s from fork: %w", fork.Repos[i].Branch, err)
		}
		if err := c.runCmdOut(ctx, fork.Repos[i].GitRoot, []string{
			"git", "branch", "--set-upstream-to", fork.Name + "/" + fork.Repos[i].Branch, fork.Repos[i].Branch,
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
		if err := c.runCmdOut(ctx, src.GitRoot, []string{
			"git", "push", "-q", fork.Name,
			src.Branch + ":refs/heads/base",
		}, stdout, stderr); err != nil {
			return nil, fmt.Errorf("push extra repo %s: %w", src.MountedPath, err)
		}
		setupCmd := "cd " + mp +
			" && git branch --track " + dstBranch + " base" +
			" && git switch -q " + dstBranch
		if err := c.runCmdOut(ctx, "", fork.SSHCommand(nil, setupCmd), stdout, stderr); err != nil {
			return nil, fmt.Errorf("setting up extra repo %s: %w", src.MountedPath, err)
		}
	}

	fork.State = "running"
	return fork, nil
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

// StatsAll fetches resource usage for multiple containers in batch (2 docker
// calls instead of 2N). Returns a map keyed by container name.
func (c *Client) StatsAll(ctx context.Context, names []string) (map[string]*ContainerStats, error) {
	result := make(map[string]*ContainerStats, len(names))
	if len(names) == 0 {
		return result, nil
	}
	var mu sync.Mutex
	var statsErr, inspectErr error

	var wg sync.WaitGroup

	// Batch docker stats (one call). Stopped containers return zeros.
	wg.Go(func() {
		args := make([]string, 0, 6+len(names))
		args = append(args, c.Runtime, "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}")
		args = append(args, names...)
		out, err := c.runCmd(ctx, "", args)
		if err != nil {
			statsErr = fmt.Errorf("docker stats: %w", err)
			return
		}
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			s, name, err := parseStatsLine(line)
			if err != nil {
				statsErr = fmt.Errorf("docker stats: %w", err)
				return
			}
			mu.Lock()
			if existing, ok := result[name]; ok {
				// Inspect goroutine may have already set DiskUsed; preserve it.
				s.DiskUsed = existing.DiskUsed
			}
			result[name] = s
			mu.Unlock()
		}
	})

	// Batch docker inspect --size (one call).
	wg.Go(func() {
		args := make([]string, 0, 5+len(names))
		args = append(args, c.Runtime, "inspect", "--size", "--format", "{{.Name}}\t{{json .SizeRw}}")
		args = append(args, names...)
		out, err := c.runCmd(ctx, "", args)
		if err != nil {
			inspectErr = fmt.Errorf("docker inspect --size: %w", err)
			return
		}
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			name := strings.TrimPrefix(parts[0], "/")
			var sz int64
			if err := json.Unmarshal([]byte(parts[1]), &sz); err != nil {
				continue
			}
			mu.Lock()
			if s, ok := result[name]; ok {
				s.DiskUsed = sz
			} else {
				result[name] = &ContainerStats{DiskUsed: sz}
			}
			mu.Unlock()
		}
	})

	wg.Wait()
	return result, errors.Join(statsErr, inspectErr)
}

// parseStatsLine parses one JSON line from docker stats output into a
// ContainerStats and returns the container name.
func parseStatsLine(line string) (*ContainerStats, string, error) {
	var raw struct {
		Name     string `json:"Name"`
		CPUPerc  string `json:"CPUPerc"`
		MemUsage string `json:"MemUsage"`
		MemPerc  string `json:"MemPerc"`
		PIDs     string `json:"PIDs"`
		NetIO    string `json:"NetIO"`
		BlockIO  string `json:"BlockIO"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, "", fmt.Errorf("parsing stats JSON: %w", err)
	}
	cpuPerc, err := parsePercent(raw.CPUPerc)
	if err != nil {
		return nil, "", fmt.Errorf("parsing CPU%%: %w", err)
	}
	memPerc, err := parsePercent(raw.MemPerc)
	if err != nil {
		return nil, "", fmt.Errorf("parsing mem%%: %w", err)
	}
	memUsed, memLimit, err := parseMemUsage(raw.MemUsage)
	if err != nil {
		return nil, "", fmt.Errorf("parsing mem usage: %w", err)
	}
	var pids int
	if raw.PIDs != "N/A" {
		pids, err = strconv.Atoi(raw.PIDs)
		if err != nil {
			return nil, "", fmt.Errorf("parsing PIDs: %w", err)
		}
	}
	netRx, netTx, err := parseIOPair(raw.NetIO)
	if err != nil {
		return nil, "", fmt.Errorf("parsing net I/O: %w", err)
	}
	blockRead, blockWrite, err := parseIOPair(raw.BlockIO)
	if err != nil {
		return nil, "", fmt.Errorf("parsing block I/O: %w", err)
	}
	return &ContainerStats{
		CPUPerc:    cpuPerc,
		MemUsed:    memUsed,
		MemLimit:   memLimit,
		MemPerc:    memPerc,
		PIDs:       pids,
		NetRx:      netRx,
		NetTx:      netTx,
		BlockRead:  blockRead,
		BlockWrite: blockWrite,
		DiskUsed:   -1,
	}, raw.Name, nil
}

// GetHostPort returns the host port mapped to a container port (e.g.
// "5901/tcp"). Returns 0 if the port is not mapped.
func (c *Container) GetHostPort(ctx context.Context, containerPort string) (int32, error) {
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", c.Name}); err != nil {
		return 0, fmt.Errorf("container %s is not running", c.Name)
	}
	return c.getHostPort(ctx, c.Name, containerPort)
}

// getHostPort extracts the host port for containerPort from a running
// container. It uses JSON output instead of Go templates to work around
// Docker 27's "index of untyped nil" bug when port bindings are nil.
func (c *Client) getHostPort(ctx context.Context, container, containerPort string) (int32, error) {
	raw, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", "--format", "{{json .NetworkSettings.Ports}}", container})
	if err != nil {
		return 0, err
	}
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return 0, fmt.Errorf("parsing port map: %w", err)
	}
	bindings := ports[containerPort]
	if len(bindings) == 0 {
		return 0, nil
	}
	port, err := strconv.ParseInt(bindings[0].HostPort, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing host port %q: %w", bindings[0].HostPort, err)
	}
	return int32(port), nil
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
		slog.DebugContext(ctx, "md", "msg", "tailscale status failed", "container", c.Name, "err", err)
		return ""
	}
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		slog.DebugContext(ctx, "md", "msg", "tailscale status JSON parse failed", "container", c.Name, "err", err)
		return ""
	}
	fqdn := strings.TrimRight(status.Self.DNSName, ".")
	if fqdn == "" {
		slog.DebugContext(ctx, "md", "msg", "tailscale FQDN empty", "container", c.Name)
	}
	return fqdn
}

// SyncDefaultBranch force-pushes the host's default branch (e.g. origin/main)
// for Repos[repoIdx] into the container so agents can diff against it.
func (c *Container) SyncDefaultBranch(ctx context.Context, repoIdx int) error {
	if len(c.Repos) == 0 {
		return errors.New("container has no repos")
	}
	if err := c.Repos[repoIdx].resolveDefaults(ctx); err != nil {
		return fmt.Errorf("sync default branch: %w", err)
	}
	r := c.Repos[repoIdx]
	// If the container's working branch is the default branch, it's already
	// synced as "base".
	if r.DefaultBranch == r.Branch {
		return nil
	}
	if _, err := c.runCmd(ctx, r.GitRoot, []string{"git", "push", "-q", "-f", c.Name, "refs/remotes/" + r.DefaultRemote + "/" + r.DefaultBranch + ":refs/heads/" + r.DefaultBranch}); err != nil {
		return fmt.Errorf("sync default branch %q: %w", r.DefaultBranch, err)
	}
	return nil
}

// prepare creates harness-specific config directories on the host so they can
// be bind-mounted into the container. Always-mounted directories
// (~/.config/agents, ~/.config/md) are created regardless.
func (c *Container) prepare(paths []AgentPaths) error {
	combined := mergePaths(paths)
	dirs := make([]string, 0, len(combined.HomePaths)+len(combined.XDGConfigPaths)+len(combined.LocalSharePaths)+len(combined.LocalStatePaths))
	for _, p := range combined.HomePaths {
		dirs = append(dirs, filepath.Join(c.Home, p))
	}
	for _, p := range combined.XDGConfigPaths {
		dirs = append(dirs, filepath.Join(c.XDGConfigHome, p))
	}
	for _, p := range combined.LocalSharePaths {
		dirs = append(dirs, filepath.Join(c.XDGDataHome, p))
	}
	for _, p := range combined.LocalStatePaths {
		dirs = append(dirs, filepath.Join(c.XDGStateHome, p))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// Ensure ~/.claude.json symlink when ~/.claude is being prepared.
	for _, p := range combined.HomePaths {
		if p == ".claude" {
			claudeJSON := filepath.Join(c.Home, ".claude.json")
			target := filepath.Join(c.Home, ".claude", "claude.json")
			if fi, err := os.Lstat(claudeJSON); err != nil {
				if !os.IsNotExist(err) {
					return fmt.Errorf("checking claude.json symlink: %w", err)
				}
				if err := os.Symlink(target, claudeJSON); err != nil {
					return fmt.Errorf("creating claude.json symlink: %w", err)
				}
			} else if fi.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("file %s exists but is not a symlink", claudeJSON)
			}
			break
		}
	}
	return nil
}

// waitForSSH runs a trivial SSH command in a retry loop until it succeeds or
// the deadline is exceeded. This confirms SSH is fully operational after the
// TCP socket opens (sshd may need a few more milliseconds to accept auth).
func (c *Container) waitForSSH(ctx context.Context, deadline time.Time) error {
	var lastErr error
	for {
		sshArgs := c.SSHCommand(nil, "true")
		cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Env = append(os.Environ(), c.env...)
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
func (c *Container) gatherGitMetadata(ctx context.Context, repo string) string {
	r := shellQuote(repo)
	cmd := "cd " + r + " && echo '=== Branch ===' && git rev-parse --abbrev-ref HEAD && echo && echo '=== Files Changed ===' && git diff --stat --cached base -- . && echo && echo '=== Recent Commits ===' && git log -5 base -- ."
	out, _ := c.runCmd(ctx, "", c.SSHCommand(nil, cmd))
	return out
}

// gatherGitDiff runs SSH to get the full patience diff from the container.
func (c *Container) gatherGitDiff(ctx context.Context, repo string) string {
	r := shellQuote(repo)
	cmd := "cd " + r + " && git diff --patience -U10 --cached base -- ."
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

// ensureImage checks whether the user image needs rebuilding and, if so,
// builds it. Returns the computed image name (keyed by base image and active
// caches). The build is serialized via Client.buildMu.
func (c *Container) ensureImage(ctx context.Context, stdout, stderr io.Writer, baseImage string, caches []CacheMount, quiet bool) (string, error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	imageName := userImageName(baseImage, activeCacheKey(caches, c.Home))
	if !c.imageBuildNeeded(ctx, imageName, baseImage, caches) {
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "- Docker image %s is up to date, skipping build.\n", imageName)
		}
		return imageName, nil
	}
	if err := c.buildSpecializedImage(ctx, stdout, stderr, imageName, baseImage, caches, agentContainerPaths(), quiet); err != nil {
		return "", err
	}
	c.invalidateImageBuildCache()
	return imageName, nil
}

func (c *Container) cleanup(ctx context.Context) {
	removeSSHConfig(ctx, filepath.Join(c.Home, ".ssh", "config.d"), c.Name)
	if len(c.Repos) > 0 {
		_, _ = c.runCmd(ctx, c.Repos[0].GitRoot, []string{"git", "remote", "remove", c.Name})
		for _, repo := range c.Repos[1:] {
			_, _ = c.runCmd(ctx, repo.GitRoot, []string{"git", "remote", "remove", c.Name})
		}
	}
	_, _ = c.runCmd(ctx, "", []string{c.Runtime, "rm", "-f", "-v", c.Name})
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

// runCmd executes a command, captures its output, and returns (stdout, error).
// If dir is non-empty, the command runs in that directory.
func (c *Client) runCmd(ctx context.Context, dir string, args []string) (string, error) {
	slog.DebugContext(ctx, "md", "msg", "exec", "cmd", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	env := append(os.Environ(), c.env...)
	env = append(env, "LANG=C")
	cmd.Env = env
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// cmdErrWithStderr wraps err with the captured stderr from an *exec.ExitError
// so that quiet-mode failures include actionable output.
func cmdErrWithStderr(prefix string, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%s: %w\n%s", prefix, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// runCmdOut executes a command, directing its stdout and stderr to the given writers.
// If dir is non-empty, the command runs in that directory.
func (c *Client) runCmdOut(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) error {
	slog.DebugContext(ctx, "md", "msg", "exec", "cmd", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	env := append(os.Environ(), c.env...)
	env = append(env, "LANG=C")
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
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

// containerJSON is the raw Docker ps JSON structure.
type containerJSON struct {
	Names     string `json:"Names"`
	State     string `json:"State"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"`
}

// containerInspectJSON is the subset of `docker inspect` output we parse.
type containerInspectJSON struct {
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
	Created string `json:"Created"`
	Config  struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
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
func unmarshalContainer(data []byte) (Container, error) {
	var raw containerJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Container{}, err
	}
	ct := Container{
		Name:  raw.Names,
		State: raw.State,
	}
	if raw.CreatedAt != "" {
		t, err := parseCreatedAt(raw.CreatedAt)
		if err != nil {
			return Container{}, err
		}
		ct.CreatedAt = t
	}
	// Docker ps outputs labels as comma-separated key=value pairs.
	for kv := range strings.SplitSeq(raw.Labels, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "md.repos":
			if data, err := base64.StdEncoding.DecodeString(v); err == nil {
				if err := json.Unmarshal(data, &ct.Repos); err != nil {
					slog.Warn("md", "msg", "failed to unmarshal repos label", "err", err)
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
	return ct, nil
}

// fillFromInspect parses docker/podman inspect JSON output and fills a Container.
//
// Both Docker and Podman inspect return a JSON array, even for a single
// container.
// The Container must already have its Client set.
func fillFromInspect(ct *Container, data []byte) error {
	var raws []containerInspectJSON
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	if len(raws) != 1 {
		return fmt.Errorf("inspect returned %d results, expected 1", len(raws))
	}
	raw := raws[0]
	ct.Name = strings.TrimPrefix(raw.Name, "/")
	ct.State = raw.State.Status
	if t, err := parseCreatedAt(raw.Created); err == nil {
		ct.CreatedAt = t
	}
	for k, v := range raw.Config.Labels {
		switch k {
		case "md.repos":
			if data, err := base64.StdEncoding.DecodeString(v); err == nil {
				if err := json.Unmarshal(data, &ct.Repos); err != nil {
					slog.Warn("md", "msg", "failed to unmarshal repos label", "err", err)
				}
				for i := range ct.Repos {
					if err := ct.Repos[i].Validate(); err != nil {
						return fmt.Errorf("inspect repos[%d]: %w", i, err)
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
	return nil
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

// runGitDir runs a git command with GIT_DIR and GIT_WORK_TREE
// explicitly set, fully decoupling git from the repository config
// (core.worktree). dir is the working directory and also used as
// GIT_WORK_TREE so git never tries to chdir to a non-existent
// submodule worktree.
func (c *Client) runGitDir(ctx context.Context, dir, gitDir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from trusted callers
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+dir, "LANG=C")
	cmd.Env = append(cmd.Env, c.env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}
