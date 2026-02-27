// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"golang.org/x/term"
)

// runCmd executes a command and returns (stdout, error).
// If capture is true, stdout/stderr are captured; otherwise they go to os.Stdout/os.Stderr.
// If dir is non-empty, the command runs in that directory.
func runCmd(ctx context.Context, dir string, args []string, capture bool) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LANG=C")
	if capture {
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return "", cmd.Run()
}

// DefaultBaseImage is the base image used when none is specified.
const DefaultBaseImage = "ghcr.io/caic-xyz/md"

// StartOpts configures container startup.
type StartOpts struct {
	// BaseImage is the full Docker image reference (e.g.
	// "ghcr.io/caic-xyz/md:v0.7.1" or "myregistry/custom:tag"). When empty,
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
	// Caches lists host directories to COPY into the image at build time.
	// Use well-known names from [WellKnownCaches] or construct [CacheMount]
	// values directly. Paths that do not exist on the host are silently skipped.
	Caches []CacheMount
	// Labels are additional Docker labels (key=value) applied to the container.
	Labels []string
	// Quiet suppresses informational output during startup.
	Quiet bool
}

// Container holds state for a single container instance.
//
// Fields marked with a label are persisted as Docker container labels
// and restored by [unmarshalContainer] when listing containers.
type Container struct {
	*Client
	// GitRoot is the absolute path to the git repository root on the host.
	// Label: md.git_root
	GitRoot string
	// RepoName is the basename of the repository directory.
	// Label: md.repo_name
	RepoName string
	// Branch is the git branch checked out in the container.
	// Label: md.branch
	Branch string
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

	// DefaultRemote is the host's default git remote (resolved lazily).
	DefaultRemote string
	// DefaultBranch is the default branch for DefaultRemote (resolved lazily).
	DefaultBranch string
}

// StartResult contains information about the started container.
type StartResult struct {
	// SSHPort is the host port mapped to the container's SSH port.
	SSHPort string
	// VNCPort is the host port mapped to the container's VNC port, if display is enabled.
	VNCPort string
	// TailscaleFQDN is the Tailscale FQDN assigned to the container, if any.
	TailscaleFQDN string
	// TailscaleAuthURL is the Tailscale auth URL when no pre-auth key was provided.
	TailscaleAuthURL string
}

// Start creates and starts a container.
func (c *Container) Start(ctx context.Context, opts *StartOpts) (_ *StartResult, retErr error) {
	// Check if container already exists.
	if _, err := runCmd(ctx, "", []string{"docker", "inspect", c.Name}, true); err == nil {
		return nil, fmt.Errorf("container %s already exists. SSH in with 'ssh %s' or clean it up via 'md kill' first",
			c.Name, c.Name)
	}

	// Generate Tailscale auth key if needed.
	var tailscaleEphemeral bool
	if opts.Tailscale && opts.TailscaleAuthKey == "" {
		key, err := generateTailscaleAuthKey(c.TailscaleAPIKey)
		if err != nil {
			if !opts.Quiet {
				_, _ = fmt.Fprintf(c.W, "- Could not generate Tailscale auth key (%v), will use browser auth\n", err)
			}
		} else {
			opts.TailscaleAuthKey = key
			tailscaleEphemeral = true
		}
	}

	baseImage := opts.BaseImage
	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	if !imageBuildNeeded(ctx, c.ImageName, baseImage, c.keysDir, c.Home, opts.Caches) {
		if !opts.Quiet {
			_, _ = fmt.Fprintf(c.W, "- Docker image %s is up to date, skipping build.\n", c.ImageName)
		}
	} else {
		if !opts.Quiet && len(opts.Caches) > 0 {
			printCacheInfo(c.W, opts.Caches, c.Home)
		}
		buildCtx, err := prepareBuildContext()
		if err != nil {
			return nil, err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(buildCtx)) }()
		if err := buildCustomizedImage(ctx, c.W, buildCtx, c.keysDir, c.ImageName, baseImage, c.Home, opts.Caches, agentContainerPaths(), opts.Quiet); err != nil {
			return nil, err
		}
	}
	result, err := runContainer(ctx, c, opts, tailscaleEphemeral)
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
// as StartOpts.Caches); nil means no caches.
func (c *Container) Run(ctx context.Context, baseImage string, command []string, caches []CacheMount) (_ int, retErr error) {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	tmp := &Container{
		Client:   c.Client,
		GitRoot:  c.GitRoot,
		RepoName: c.RepoName,
		Branch:   c.Branch,
		Name:     fmt.Sprintf("md-%s-run-%x", sanitizeDockerName(c.RepoName), buf),
	}

	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	if imageBuildNeeded(ctx, c.ImageName, baseImage, c.keysDir, c.Home, caches) {
		buildCtx, err := prepareBuildContext()
		if err != nil {
			return 1, err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(buildCtx)) }()
		if err := buildCustomizedImage(ctx, c.W, buildCtx, c.keysDir, c.ImageName, baseImage, c.Home, caches, agentContainerPaths(), true); err != nil {
			return 1, err
		}
	}
	opts := StartOpts{Quiet: true}
	if _, err := runContainer(ctx, tmp, &opts, false); err != nil {
		tmp.cleanup(ctx)
		return 1, err
	}

	cmdStr := strings.Join(command, " ")
	_, err := runCmd(ctx, "", c.SSHCommand(tmp.Name, "cd ~/src/"+shellQuote(c.RepoName)+" && "+cmdStr), false)
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

// Kill stops and removes the container.
func (c *Container) Kill(ctx context.Context) error {
	_, containerErr := runCmd(ctx, "", []string{"docker", "inspect", c.Name}, true)
	containerExists := containerErr == nil
	_, remoteErr := gitutil.RunGit(ctx, c.GitRoot, "remote", "get-url", c.Name)
	remoteExists := remoteErr == nil
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	sshConf := filepath.Join(sshConfigDir, c.Name+".conf")
	sshKnown := filepath.Join(sshConfigDir, c.Name+".known_hosts")
	_, sshConfErr := os.Stat(sshConf)
	_, sshKnownErr := os.Stat(sshKnown)
	sshExists := sshConfErr == nil || sshKnownErr == nil

	if !containerExists && !remoteExists && !sshExists {
		return fmt.Errorf("%s not found", c.Name)
	}

	// Clean up non-ephemeral Tailscale node.
	if containerExists {
		if !c.Tailscale {
			tsLabel, _ := runCmd(ctx, "", []string{"docker", "inspect", "--format", `{{index .Config.Labels "md.tailscale"}}`, c.Name}, true)
			c.Tailscale = tsLabel == "1"
		}
		if c.Tailscale {
			ephLabel, _ := runCmd(ctx, "", []string{"docker", "inspect", "--format", `{{index .Config.Labels "md.tailscale_ephemeral"}}`, c.Name}, true)
			if ephLabel != "1" {
				statusJSON, err := runCmd(ctx, "", []string{"docker", "exec", c.Name, "tailscale", "status", "--json"}, true)
				if err == nil {
					var status tailscaleStatus
					if json.Unmarshal([]byte(statusJSON), &status) == nil && status.Self.ID != "" {
						_, _ = fmt.Fprintln(c.W, "- Removing Tailscale node from tailnet...")
						deleteTailscaleDevice(c.TailscaleAPIKey, status.Self.ID)
					}
				}
			}
		}
	}

	_ = os.Remove(sshConf)
	_ = os.Remove(sshKnown)

	var retErr error
	if remoteExists {
		if _, err := gitutil.RunGit(ctx, c.GitRoot, "remote", "remove", c.Name); err != nil {
			retErr = err
		}
	}
	if containerExists {
		if _, err := runCmd(ctx, "", []string{"docker", "rm", "-f", "-v", c.Name}, true); err != nil {
			retErr = err
		}
	}
	_, _ = fmt.Fprintf(c.W, "Removed %s\n", c.Name)
	return retErr
}

// Push force-pushes local state into the container.
func (c *Container) Push(ctx context.Context) error {
	if err := c.checkContainerState(ctx); err != nil {
		return err
	}
	if err := c.SyncDefaultBranch(ctx); err != nil {
		return err
	}
	// Refuse if there are pending local changes on the branch being pushed.
	currentBranch, _ := gitutil.RunGit(ctx, c.GitRoot, "branch", "--show-current")
	if currentBranch == c.Branch {
		if _, err := gitutil.RunGit(ctx, c.GitRoot, "diff", "--quiet", "--exit-code"); err != nil {
			return errors.New("there are pending changes locally. Please commit or stash them before pushing")
		}
	}
	repo := shellQuote(c.RepoName)
	branch := shellQuote(c.Branch)
	// Commit any pending changes in the container.
	_, _ = runCmd(ctx, "", c.SSHCommand(c.Name, "cd ~/src/"+repo+" && git add . && (git diff --quiet HEAD -- . || git commit -q -m 'Backup before push')"), true)
	containerCommit, _ := runCmd(ctx, "", c.SSHCommand(c.Name, "cd ~/src/"+repo+" && git rev-parse HEAD"), true)
	backupBranch := "backup-" + time.Now().Format("20060102-150405")
	_, _ = runCmd(ctx, "", c.SSHCommand(c.Name, "cd ~/src/"+repo+" && git branch -f "+backupBranch+" "+containerCommit), true)
	_, _ = fmt.Fprintf(c.W, "- Previous state saved as git branch: %s\n", backupBranch)
	if _, err := runCmd(ctx, c.GitRoot, []string{"git", "push", "-q", "-f", "--tags", c.Name, c.Branch + ":base"}, false); err != nil {
		return err
	}
	if _, err := runCmd(ctx, "", c.SSHCommand(c.Name, "cd ~/src/"+repo+" && git switch -q -C "+branch+" base"), false); err != nil {
		return err
	}
	// Update the local remote-tracking ref so it reflects the pushed state.
	if _, err := runCmd(ctx, c.GitRoot, []string{"git", "update-ref", "refs/remotes/" + c.Name + "/" + c.Branch, c.Branch}, false); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(c.W, "- Container updated.")
	return nil
}

// Fetch commits any uncommitted changes in the container and fetches them
// locally, updating the remote-tracking ref without integrating.
//
// provider and model control AI commit message generation. See https://github.com/maruel/genai for valid
// names. If provider is empty, a default message is used.
func (c *Container) Fetch(ctx context.Context, provider, model string) error {
	if err := c.checkContainerState(ctx); err != nil {
		return err
	}
	if err := c.SyncDefaultBranch(ctx); err != nil {
		return err
	}
	repo := shellQuote(c.RepoName)
	// Check if there are uncommitted changes in the container.
	if _, err := runCmd(ctx, "", c.SSHCommand(c.Name, "cd ~/src/"+repo+" && git add . && git diff --quiet HEAD -- ."), true); err != nil {
		commitMsg := "Pull from md"
		if provider != "" {
			if p, err := newProvider(ctx, provider, model); err != nil {
				slog.WarnContext(ctx, "failed to initialize provider", "err", err)
			} else {
				metadata := c.gatherGitMetadata(ctx, c.Name, repo)
				diff := c.gatherGitDiff(ctx, c.Name, repo)
				if msg, err := gitutil.GenerateCommitMsg(ctx, p, metadata, diff, nil); err != nil {
					slog.WarnContext(ctx, "failed to generate commit message", "err", err)
				} else if msg != "" {
					commitMsg = msg
				}
			}
		}
		gitUserName, _ := gitutil.RunGit(ctx, c.GitRoot, "config", "user.name")
		gitUserEmail, _ := gitutil.RunGit(ctx, c.GitRoot, "config", "user.email")
		gitAuthor := shellQuote(gitUserName + " <" + gitUserEmail + ">")
		commitCmd := "cd ~/src/" + repo + " && echo " + shellQuote(commitMsg) + " | git commit -a -q --author " + gitAuthor + " -F -"
		if _, err := runCmd(ctx, "", c.SSHCommand(c.Name, commitCmd), false); err != nil {
			return fmt.Errorf("committing in container: %w", err)
		}
	}
	_, err := runCmd(ctx, c.GitRoot, []string{"git", "fetch", "-q", c.Name, c.Branch}, false)
	return err
}

// Pull fetches changes from the container and integrates them into the local branch.
//
// provider and model control AI commit message generation. See https://github.com/maruel/genai for valid
// names. If provider is empty, a default message is used.
func (c *Container) Pull(ctx context.Context, provider, model string) error {
	if err := c.Fetch(ctx, provider, model); err != nil {
		return err
	}
	remoteRef := c.Name + "/" + c.Branch
	currentBranch, _ := gitutil.RunGit(ctx, c.GitRoot, "branch", "--show-current")
	if currentBranch == c.Branch {
		// Already on the branch, rebase locally.
		if _, err := runCmd(ctx, c.GitRoot, []string{"git", "rebase", "-q", remoteRef}, false); err != nil {
			return err
		}
	} else if _, err := gitutil.RunGit(ctx, c.GitRoot, "merge-base", "--is-ancestor", c.Branch, remoteRef); err == nil {
		// Fast-forward: update ref without checkout.
		if _, err := runCmd(ctx, c.GitRoot, []string{"git", "update-ref", "refs/heads/" + c.Branch, remoteRef}, false); err != nil {
			return err
		}
	} else {
		// Not a fast-forward. Checkout the branch, rebase, then checkout back.
		origRef := currentBranch
		if origRef == "" {
			origRef, _ = gitutil.RunGit(ctx, c.GitRoot, "rev-parse", "HEAD")
		}
		if _, err := runCmd(ctx, c.GitRoot, []string{"git", "checkout", "-q", c.Branch}, false); err != nil {
			return err
		}
		if _, err := runCmd(ctx, c.GitRoot, []string{"git", "rebase", "-q", remoteRef}, false); err != nil {
			_, _ = runCmd(ctx, c.GitRoot, []string{"git", "checkout", "-q", origRef}, false)
			return err
		}
		if _, err := runCmd(ctx, c.GitRoot, []string{"git", "checkout", "-q", origRef}, false); err != nil {
			return err
		}
	}
	_, err := runCmd(ctx, c.GitRoot, []string{"git", "push", "-q", "-f", c.Name, c.Branch + ":base"}, false)
	return err
}

// Diff writes the diff between base and current in the container.
// When stdout is a terminal, a TTY is allocated so git's pager and colors work.
func (c *Container) Diff(ctx context.Context, stdout, stderr io.Writer, extraArgs []string) error {
	if err := c.checkContainerState(ctx); err != nil {
		return err
	}
	if err := c.SyncDefaultBranch(ctx); err != nil {
		return err
	}
	quotedArgs := make([]string, len(extraArgs))
	for i, a := range extraArgs {
		quotedArgs[i] = shellQuote(a)
	}
	sshArgs := c.SSHCommand("-q")
	cmd := exec.CommandContext(ctx, sshArgs[0])
	if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		sshArgs = append(sshArgs, "-t")
		cmd.Stdin = os.Stdin
	}
	sshArgs = append(sshArgs, c.Name, "cd ~/src/"+shellQuote(c.RepoName)+" && git add . && git diff base "+strings.Join(quotedArgs, " ")+" -- .")
	cmd.Path, _ = exec.LookPath(sshArgs[0])
	cmd.Args = sshArgs
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// GetHostPort returns the host port mapped to a container port (e.g.
// "5901/tcp"). Returns empty string if the port is not mapped.
func (c *Container) GetHostPort(ctx context.Context, containerPort string) (string, error) {
	if _, err := runCmd(ctx, "", []string{"docker", "inspect", c.Name}, true); err != nil {
		return "", fmt.Errorf("container %s is not running", c.Name)
	}
	return getHostPort(ctx, c.Name, containerPort)
}

// getHostPort extracts the host port for containerPort from a running
// container. It uses JSON output instead of Go templates to work around
// Docker 27's "index of untyped nil" bug when port bindings are nil.
func getHostPort(ctx context.Context, container, containerPort string) (string, error) {
	raw, err := runCmd(ctx, "", []string{"docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", container}, true)
	if err != nil {
		return "", err
	}
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return "", fmt.Errorf("parsing port map: %w", err)
	}
	bindings := ports[containerPort]
	if len(bindings) == 0 {
		return "", nil
	}
	return bindings[0].HostPort, nil
}

// tailscaleStatus is the subset of `tailscale status --json` we care about.
type tailscaleStatus struct {
	Self struct {
		ID      string `json:"ID"`
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

// TailscaleFQDN returns the Tailscale FQDN for the container, or "" if unavailable.
func (c *Container) TailscaleFQDN(ctx context.Context) string {
	if !c.Tailscale || c.State != "running" {
		return ""
	}
	statusJSON, err := runCmd(ctx, "", []string{"docker", "exec", c.Name, "tailscale", "status", "--json"}, true)
	if err != nil {
		return ""
	}
	var status tailscaleStatus
	if json.Unmarshal([]byte(statusJSON), &status) != nil {
		return ""
	}
	return strings.TrimRight(status.Self.DNSName, ".")
}

// containerJSON is the raw Docker ps JSON structure.
type containerJSON struct {
	Names     string `json:"Names"`
	State     string `json:"State"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"`
}

// unmarshalContainer parses docker ps JSON output, converting the CreatedAt
// timestamp string into a time.Time and extracting md.* labels.
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
		// Docker uses "2006-01-02 15:04:05 -0700 MST" format.
		t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", raw.CreatedAt)
		if err != nil {
			return Container{}, fmt.Errorf("parsing CreatedAt %q: %w", raw.CreatedAt, err)
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
		case "md.git_root":
			ct.GitRoot = v
		case "md.repo_name":
			ct.RepoName = v
		case "md.branch":
			ct.Branch = v
		case "md.display":
			ct.Display = v == "1"
		case "md.tailscale":
			ct.Tailscale = v == "1"
		case "md.usb":
			ct.USB = v == "1"
		}
	}
	return ct, nil
}

// resolveDefaults populates DefaultRemote and DefaultBranch if not already set.
func (c *Container) resolveDefaults(ctx context.Context) error {
	if c.DefaultRemote == "" {
		remote, err := gitutil.DefaultRemote(ctx, c.GitRoot)
		if err != nil {
			return err
		}
		c.DefaultRemote = remote
	}
	if c.DefaultBranch == "" {
		branch, err := gitutil.DefaultBranch(ctx, c.GitRoot, c.DefaultRemote)
		if err != nil {
			return err
		}
		c.DefaultBranch = branch
	}
	return nil
}

// SyncDefaultBranch force-pushes the host's default branch (e.g. origin/main)
// into the container so agents can diff against it.
func (c *Container) SyncDefaultBranch(ctx context.Context) error {
	if err := c.resolveDefaults(ctx); err != nil {
		return fmt.Errorf("sync default branch: %w", err)
	}
	// If the container's working branch is the default branch, it's already
	// synced as "base".
	if c.DefaultBranch == c.Branch {
		return nil
	}
	if _, err := gitutil.RunGit(ctx, c.GitRoot, "push", "-q", "-f", c.Name, "refs/remotes/"+c.DefaultRemote+"/"+c.DefaultBranch+":refs/heads/"+c.DefaultBranch); err != nil {
		return fmt.Errorf("sync default branch %q: %w", c.DefaultBranch, err)
	}
	return nil
}

func (c *Container) checkContainerState(ctx context.Context) error {
	_, containerErr := runCmd(ctx, "", []string{"docker", "inspect", c.Name}, true)
	containerExists := containerErr == nil
	_, remoteErr := gitutil.RunGit(ctx, c.GitRoot, "remote", "get-url", c.Name)
	remoteExists := remoteErr == nil
	sshConfigDir := filepath.Join(c.Home, ".ssh", "config.d")
	_, sshErr := os.Stat(filepath.Join(sshConfigDir, c.Name+".conf"))
	sshExists := sshErr == nil

	if !containerExists && !remoteExists && !sshExists {
		return fmt.Errorf("no container running for branch '%s'.\nStart a container with: md start", c.Branch)
	}
	if !containerExists || !remoteExists || !sshExists {
		var issues []string
		if !containerExists {
			issues = append(issues, "Docker container is not running")
		}
		if !remoteExists {
			issues = append(issues, "Git remote is missing")
		}
		if !sshExists {
			issues = append(issues, "SSH config is missing")
		}
		return fmt.Errorf("inconsistent state detected for %s:\n  - %s\nConsider running 'md kill' to clean up, then 'md start' to restart",
			c.Name, strings.Join(issues, "\n  - "))
	}
	return nil
}

func (c *Container) cleanup(ctx context.Context) {
	removeSSHConfig(filepath.Join(c.Home, ".ssh", "config.d"), c.Name)
	_, _ = gitutil.RunGit(ctx, c.GitRoot, "remote", "remove", c.Name)
	_, _ = runCmd(ctx, "", []string{"docker", "rm", "-f", "-v", c.Name}, true)
}

// newProvider creates a genai.Provider for the given provider name and model.
func newProvider(ctx context.Context, provider, model string) (genai.Provider, error) {
	cfg, ok := providers.All[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	m := genai.ProviderOptionModel(model)
	if m == "" {
		m = genai.ModelCheap
	}
	return cfg.Factory(ctx, m)
}

// gatherGitMetadata runs SSH commands to collect branch, stat, and log from
// the container. This data is always small.
func (c *Client) gatherGitMetadata(ctx context.Context, containerName, repo string) string {
	cmd := "cd ~/src/" + repo + " && echo '=== Branch ===' && git rev-parse --abbrev-ref HEAD && echo && echo '=== Files Changed ===' && git diff --stat --cached base -- . && echo && echo '=== Recent Commits ===' && git log -5 base -- ."
	out, _ := runCmd(ctx, "", c.SSHCommand(containerName, cmd), true)
	return out
}

// gatherGitDiff runs SSH to get the full patience diff from the container.
func (c *Client) gatherGitDiff(ctx context.Context, containerName, repo string) string {
	cmd := "cd ~/src/" + repo + " && git diff --patience -U10 --cached base -- ."
	out, _ := runCmd(ctx, "", c.SSHCommand(containerName, cmd), true)
	return out
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
