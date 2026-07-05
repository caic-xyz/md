// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Package main implements the md CLI for isolated Docker development containers.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"golang.org/x/sync/errgroup"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/containers"
	"github.com/caic-xyz/md/git"
)

type app struct {
	runtimeOverride      string
	controlMasterEnabled bool
	client               *md.Client
}

func main() {
	if err := mainImpl(); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.code)
		}
		fmt.Fprintf(os.Stderr, "md: %v\n", err)
		os.Exit(1)
	}
}

// addVerboseFlag registers -v/-verbose on fs and returns the bool pointer.
func addVerboseFlag(fs *flag.FlagSet) *bool {
	v := fs.Bool("verbose", false, "Enable debug logging")
	fs.BoolVar(v, "v", false, "Enable debug logging")
	return v
}

// initLogging configures the default slog handler based on the verbose flag.
func initLogging(verbose bool) {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func mainImpl() (retErr error) {
	a := &app{}
	defer func() {
		retErr = errors.Join(retErr, a.Close())
	}()
	// Pre-parse to support flags before the subcommand (e.g. "md -v start").
	pre := flag.NewFlagSet("md", flag.ContinueOnError)
	preVerbose := addVerboseFlag(pre)
	preRuntime := pre.String("runtime", "", "Container runtime: docker or podman (default: auto-detect)")
	preControlMaster := pre.Bool("control-master", false, "Enable SSH ControlMaster connection multiplexing")
	// Ignore errors: unknown flags here are subcommand flags, parsed later.
	_ = pre.Parse(os.Args[1:])
	initLogging(*preVerbose)
	a.runtimeOverride = *preRuntime
	a.controlMasterEnabled = *preControlMaster && runtime.GOOS != "windows"
	remaining := pre.Args()

	if len(remaining) == 0 {
		usage()
		return errors.New("no command specified")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd := remaining[0]
	args := remaining[1:]
	switch cmd {
	case "build-image":
		return a.cmdBuildImage(ctx, args)
	case "diff":
		return a.cmdDiff(ctx, args)
	case "fork":
		return a.cmdFork(ctx, args)
	case "list":
		return a.cmdList(ctx, args)
	case "prune":
		return a.cmdPrune(ctx, args)
	case "pull":
		return a.cmdPull(ctx, args)
	case "purge", "kill":
		return a.cmdPurge(ctx, args)
	case "push":
		return a.cmdPush(ctx, args)
	case "run":
		return a.cmdRun(ctx, args)
	case "ssh":
		return cmdSSH(args)
	case "start":
		return a.cmdStart(ctx, args)
	case "stop":
		return a.cmdStop(ctx, args)
	case "sudo-password":
		return a.cmdSudoPassword(ctx, args)
	case "version":
		return cmdVersion(args)
	case "vnc":
		return a.cmdVNC(ctx, args)
	case "help", "-h", "-help", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func usage() {
	_, _ = fmt.Fprint(os.Stderr, "md (my devenv): local development environment with git clone\n"+
		"\n"+
		"Global flags:\n"+
		"  -v, -verbose       Enable debug logging\n"+
		"  --runtime <name>   Container runtime: docker or podman (default: auto-detect)\n"+
		"\n"+
		"Commands:\n"+
		"  build-image   Build the base Docker image locally\n"+
		"  diff          Show differences between host branch and current changes\n"+
		"  fork          Snapshot container and create a new one on forked branches\n"+
		"  list          List running md containers\n"+
		"  prune         Remove unused md images and build cache\n"+
		"  pull          Pull changes from container back to local branch\n"+
		"  purge         Stop and remove the container permanently\n"+
		"  push          Force-push current repo state into the running container\n"+
		"  run <cmd>     Start a temporary container, run a command, then clean up\n"+
		"  ssh           (see 'ssh md-<repo>-<branch>')\n"+
		"  start         Pull base image, rebuild if needed, start container, open shell\n"+
		"  stop          Stop the container (preserves filesystem for later revival)\n"+
		"  sudo-password Print the container's random sudo password\n"+
		"  version       Print version information\n"+
		"  vnc           Open VNC connection to the container\n")
}

type containerFlags struct {
	image    *string
	tag      *string
	platform *string
	branch   *string
	repo     *string
}

// addContainerFlags registers -b/-branch and -repo on the given FlagSet.
// When image is true, --image and --tag are also registered.
func addContainerFlags(fs *flag.FlagSet, image bool) *containerFlags {
	cf := &containerFlags{}
	if image {
		cf.image = fs.String("image", "", "Full base Docker image; md build-image generates md-user-local (default: "+md.DefaultBaseImage+":latest)")
		cf.tag = fs.String("tag", "", "Tag for the default base image ("+md.DefaultBaseImage+":<tag>)")
		cf.platform = fs.String("platform", "", "Container platform: linux/amd64 or linux/arm64 (default: "+md.DefaultPlatform().String()+")")
	}
	cf.branch = fs.String("branch", "", "Branch to use (default: current branch)")
	fs.StringVar(cf.branch, "b", "", "Branch to use (default: current branch)")
	cf.repo = fs.String("repo", "", "Path to git repository (default: current directory)")
	fs.StringVar(cf.repo, "r", "", "Path to git repository (default: current directory)")
	return cf
}

func (cf *containerFlags) containerPlatform() (string, error) {
	if cf.platform == nil {
		return "", nil
	}
	platform := md.Platform(*cf.platform).Resolve()
	if err := platform.Validate(); err != nil {
		return "", err
	}
	return platform.String(), nil
}

// baseImage returns the resolved base image from --image and --tag flags.
// --image takes precedence; --tag expands to DefaultBaseImage+":<tag>".
// Returns empty string when neither is set (caller should use DefaultBaseImage).
func (cf *containerFlags) baseImage() (string, error) {
	hasImage := cf.image != nil && *cf.image != ""
	hasTag := cf.tag != nil && *cf.tag != ""
	if hasImage && hasTag {
		return "", errors.New("--image and --tag are mutually exclusive")
	}
	if hasImage {
		return *cf.image, nil
	}
	if hasTag {
		return md.DefaultBaseImage + ":" + *cf.tag, nil
	}
	return "", nil
}

func (a *app) Close() error {
	if a.client == nil {
		return nil
	}
	return a.client.Close()
}

func (a *app) newClient() (*md.Client, error) {
	if a.runtimeOverride != "" && a.runtimeOverride != "docker" && a.runtimeOverride != "podman" {
		return nil, fmt.Errorf("--runtime must be \"docker\" or \"podman\", got %q", a.runtimeOverride)
	}
	if a.client != nil {
		return a.client, nil
	}
	logger := slog.Default()
	var rt containers.Runtime
	if a.runtimeOverride != "" {
		var err error
		rt, err = containers.New(a.runtimeOverride, logger, nil)
		if err != nil {
			return nil, err
		}
	}
	c, err := md.New(logger, rt, os.Stdout)
	if err != nil {
		return nil, err
	}
	c.ControlMaster = a.controlMasterEnabled
	c.GithubToken = os.Getenv("GITHUB_TOKEN")
	c.TailscaleAPIKey = os.Getenv("TAILSCALE_API_KEY")
	a.client = c
	return c, nil
}

// findContainerAndRepo searches all containers for one that contains the
// repo identified by cf (defaults to cwd). Returns the container and the
// index of the matched repo within it. If cf.branch is set, it is used to
// disambiguate when multiple containers share the same git root.
func (a *app) findContainerAndRepo(ctx context.Context, cf *containerFlags) (*md.Container, int, error) {
	c, err := a.newClient()
	if err != nil {
		return nil, 0, err
	}
	var searchPath string
	if cf.repo != nil && *cf.repo != "" {
		searchPath = *cf.repo
	} else {
		searchPath, err = os.Getwd()
		if err != nil {
			return nil, 0, err
		}
	}
	g, err := git.RootDir(ctx, searchPath, c.Logger)
	if err != nil {
		return nil, 0, fmt.Errorf("not in a git repository: %w", err)
	}
	branch := ""
	if cf.branch != nil {
		branch = *cf.branch
	}
	// If no branch was specified, use the current local branch as the default
	// disambiguator so that two containers on different branches of the same
	// repo are resolved automatically.
	if branch == "" {
		branch, _ = g.RunGit(ctx, "branch", "--show-current")
	}
	cts, err := c.List(ctx)
	if err != nil {
		return nil, 0, err
	}
	var matched []*md.Container
	var matchedIdx []int
	for _, ct := range cts {
		for i, repo := range ct.Repos {
			if repo.GitRoot == g.Root && (branch == "" || repo.Branch == branch) {
				matched = append(matched, ct)
				matchedIdx = append(matchedIdx, i)
				break
			}
		}
	}
	switch len(matched) {
	case 0:
		return nil, 0, fmt.Errorf("no container found for %s", g.Root)
	case 1:
		return matched[0], matchedIdx[0], nil
	default:
		names := make([]string, len(matched))
		for i, ct := range matched {
			names[i] = ct.Name
		}
		return nil, 0, fmt.Errorf("multiple containers match %s: %s; use -branch to disambiguate", g.Root, strings.Join(names, ", "))
	}
}

// newContainer resolves a Container from flags. extraRepoSpecs holds
// additional "path[:branch]" strings (e.g. from -extra-repo in cmdStart).
func (a *app) newContainer(ctx context.Context, cf *containerFlags, extraRepoSpecs []string) (*md.Container, error) {
	c, err := a.newClient()
	if err != nil {
		return nil, err
	}
	// Resolve primary repo.
	var repos []md.Repo
	var primaryPath string
	if cf.repo != nil && *cf.repo != "" {
		primaryPath = *cf.repo
	} else {
		primaryPath, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	g, gitErr := git.RootDir(ctx, primaryPath, c.Logger)
	if gitErr == nil {
		// Chdir so that relative paths in subsequent flag resolution (e.g.
		// -extra-repo) resolve from the git root. Safe because the CLI is serial.
		if err := os.Chdir(g.Root); err != nil {
			return nil, err
		}
		var branch string
		if cf.branch != nil && *cf.branch != "" {
			branch = *cf.branch
		} else {
			branch, err = g.CurrentBranch(ctx)
			if err != nil {
				return nil, fmt.Errorf("detached HEAD in %s: check out a named branch or use -b to specify one", g.Root)
			}
		}
		repos = append(repos, md.Repo{GitRoot: g.Root, Branch: branch})
	} else if cf.repo != nil && *cf.repo != "" {
		// Explicit -repo that isn't a git root is an error.
		return nil, fmt.Errorf("repo %s: %w", primaryPath, gitErr)
	}
	// Not in a git repo and no explicit -repo: create a no-repo container.
	// Resolve extra repos.
	extra, err := resolveRepoSpecs(ctx, c.Logger, extraRepoSpecs)
	if err != nil {
		return nil, err
	}
	repos = append(repos, extra...)
	return c.Container(repos...)
}

// resolveRepoSpecs resolves "path[:branch]" specs into Repos.
func resolveRepoSpecs(ctx context.Context, logger md.Logger, specs []string) ([]md.Repo, error) {
	repos := make([]md.Repo, 0, len(specs))
	for _, spec := range specs {
		path, branch, _ := strings.Cut(spec, ":")
		g, err := git.RootDir(ctx, path, logger)
		if err != nil {
			return nil, fmt.Errorf("extra repo %s: %w", path, err)
		}
		if branch == "" {
			branch, err = g.CurrentBranch(ctx)
			if err != nil {
				return nil, fmt.Errorf("extra repo %s: %w", path, err)
			}
		}
		repos = append(repos, md.Repo{GitRoot: g.Root, Branch: branch})
	}
	return repos, nil
}

// ensureGithubToken populates c.GithubToken from `gh auth token` if
// GITHUB_TOKEN was not set. Returns true if a token is available.
func ensureGithubToken(ctx context.Context, c *md.Client) bool {
	if c.GithubToken == "" {
		if _, err := exec.LookPath("gh"); err == nil {
			if out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output(); err == nil {
				c.GithubToken = strings.TrimSpace(string(out))
			}
		}
	}
	return c.GithubToken != ""
}

// resolveGithubToken returns the GitHub token to inject into the container
// when github is true. Returns "" when false.
func resolveGithubToken(ctx context.Context, c *md.Client, github bool) (string, error) {
	if !github {
		return "", nil
	}
	if !ensureGithubToken(ctx, c) {
		return "", errors.New("--github requires a GitHub token; set GITHUB_TOKEN or authenticate with `gh auth login`")
	}
	return c.GithubToken, nil
}

func (a *app) cmdStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	display := fs.Bool("display", false, "Enable X11/VNC display")
	fs.BoolVar(display, "d", false, "Enable X11/VNC display")
	tailscale := fs.Bool("tailscale", false, "Enable Tailscale networking")
	usb := fs.Bool("usb", false, "Pass through USB devices (/dev/bus/usb)")
	sudoFlag := fs.Bool("sudo", false, "Enable root access via sudo (random per-container password)")
	cf := addContainerFlags(fs, true)
	extraRepos := &stringSlice{}
	fs.Var(extraRepos, "extra-repo", "Additional git repository path[:branch] to map; may be repeated")
	fs.Var(extraRepos, "e", "Additional git repository path[:branch] to map; may be repeated")
	noSSH := fs.Bool("no-ssh", false, "Don't SSH into the container after starting")
	quiet := fs.Bool("q", false, "Suppress informational messages")
	labels := &stringSlice{}
	fs.Var(labels, "label", "Set Docker container label (key=value); can be repeated")
	fs.Var(labels, "l", "Set Docker container label (key=value); can be repeated")
	cacheSpecs := &stringSlice{}
	fs.Var(cacheSpecs, "cache", "Add a cache: well-known name or host:container[:ro]; may be repeated")
	mountSpecs := &stringSlice{}
	fs.Var(mountSpecs, "mount", "Bind-mount host:container[:ro]; may be repeated")
	noCacheSpecs := &stringSlice{}
	fs.Var(noCacheSpecs, "no-cache", "Exclude a default well-known cache by name; may be repeated")
	noCaches := fs.Bool("no-caches", false, "Disable all default caches")
	github := fs.Bool("github", false, "Inject GitHub token into container")
	cpus := fs.Int("cpus", md.DefaultMaxCPUs(), "Max CPU cores for the container (0=no limit)")
	dockerFlags := &shellSplitSlice{}
	fs.Var(dockerFlags, "docker-flag", "Extra flag passed verbatim to docker/podman run; may be repeated")
	fs.Usage = func() { printSubcommandUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}

	ct, err := a.newContainer(ctx, cf, extraRepos.values)
	if err != nil {
		return err
	}
	baseImage, err := cf.baseImage()
	if err != nil {
		return err
	}
	platform, err := cf.containerPlatform()
	if err != nil {
		return err
	}
	caches, err := resolveCaches(cacheSpecs.values, noCacheSpecs.values, *noCaches)
	if err != nil {
		return err
	}
	mounts, err := resolveMounts(mountSpecs.values)
	if err != nil {
		return err
	}
	harnessMounts, err := ct.AgentMounts(slices.Collect(maps.Values(md.HarnessMounts))...)
	if err != nil {
		return err
	}
	mounts = append(harnessMounts, mounts...)
	githubToken, err := resolveGithubToken(ctx, ct.Client, *github)
	if err != nil {
		return err
	}
	var extraEnv []string
	if githubToken != "" {
		extraEnv = append(extraEnv, "GITHUB_TOKEN="+githubToken)
	}
	opts := md.StartOpts{
		BaseImage:        baseImage,
		Platform:         platform,
		Display:          *display,
		Tailscale:        *tailscale,
		USB:              *usb,
		Sudo:             *sudoFlag,
		TailscaleAuthKey: os.Getenv("TAILSCALE_AUTHKEY"),
		Caches:           caches,
		Mounts:           mounts,
		Labels:           labels.values,
		Quiet:            *quiet,
		ExtraEnv:         extraEnv,
		MaxCPUs:          *cpus,
		ExtraRunArgs:     dockerFlags.values,
	}
	switch ct.Status(ctx) {
	case "exited", "stopped":
		if !*quiet {
			_, _ = fmt.Fprintf(os.Stdout, "- Reviving stopped container %s ...\n", ct.Name)
		}
		if err := ct.Revive(ctx, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("reviving %s: %w", ct.Name, err)
		}
		if !*quiet {
			if err := printContainerSummary(ctx, ct, nil, "- Revived "+ct.Name); err != nil {
				return err
			}
		}
	case "running":
		return fmt.Errorf("container %s is already running. SSH in with: ssh %s", ct.Name, ct.Name)
	default:
		// Container does not exist, proceed with normal launch.
		if err := ct.Launch(ctx, os.Stdout, os.Stderr, &opts); err != nil {
			return err
		}
		result, err := ct.Connect(ctx, os.Stdout, os.Stderr, &opts)
		if err != nil {
			return err
		}
		if !*quiet {
			if err := printContainerSummary(ctx, ct, result, "- Created "+ct.Name); err != nil {
				return err
			}
		}
	}
	if !*noSSH {
		sshArgs := ct.SSHCommand(nil, "")
		slog.DebugContext(ctx, "md", "msg", "ssh", "container", ct.Name, "cmd", sshArgs)
		cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

func printContainerSummary(ctx context.Context, ct *md.Container, r *md.StartResult, header string) error {
	fmt.Println(header)
	// Tailscale info only available after Connect (start, not fork).
	if r != nil {
		if r.TailscaleFQDN != "" {
			fmt.Printf("  >  Tailscale FQDN: %s\n", r.TailscaleFQDN)
		}
		if r.TailscaleAuthURL != "" {
			fmt.Printf("  >  Tailscale auth: %s\n", r.TailscaleAuthURL)
		}
	}
	if len(ct.Repos) > 0 {
		for _, r := range ct.Repos {
			fmt.Printf("  > Repo %s on branch '%s'\n", filepath.Base(r.MountedPath), r.Branch)
		}
		fmt.Println("  > Host state is mapped to the branch upstream")
		fmt.Println("  > See changes (in container): git diff @{upstream}")
		fmt.Println("  > See changes (on host)     : md diff")
	}
	fmt.Println("  > Stop container            : md stop")
	fmt.Println("  > Purge container           : md purge")
	fmt.Printf("  > SSH                       : ssh %s\n", ct.Name)
	if ct.VNCPort != 0 {
		fmt.Printf("  > VNC to localhost:%-5d or : md vnc\n", ct.VNCPort)
	}
	if ct.Sudo {
		if pw, err := ct.SudoPassword(ctx); err != nil {
			return err
		} else {
			fmt.Printf("  > Sudo password             : %s\n", pw)
		}
	}
	return nil
}

func (a *app) cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, true)
	cacheSpecs := &stringSlice{}
	fs.Var(cacheSpecs, "cache", "Add a cache: well-known name or host:container[:ro]; may be repeated")
	mountSpecs := &stringSlice{}
	fs.Var(mountSpecs, "mount", "Bind-mount host:container[:ro]; may be repeated")
	noCacheSpecs := &stringSlice{}
	fs.Var(noCacheSpecs, "no-cache", "Exclude a default well-known cache by name; may be repeated")
	noCaches := fs.Bool("no-caches", false, "Disable all default caches")
	extraRepos := &stringSlice{}
	fs.Var(extraRepos, "extra-repo", "Additional git repository path[:branch] to map; may be repeated")
	fs.Var(extraRepos, "e", "Additional git repository path[:branch] to map; may be repeated")
	github := fs.Bool("github", false, "Inject GitHub token into container")
	envSpecs := &stringSlice{}
	fs.Var(envSpecs, "env", "Set environment in container: NAME copies host, NAME=value sets, NAME= unsets; may be repeated")
	applyPatch := fs.Bool("apply-patch", false, "Pull changes from the temporary container back to the host after the command")
	cpus := fs.Int("cpus", md.DefaultMaxCPUs(), "Max CPU cores for the container (0=no limit)")
	dockerFlags := &shellSplitSlice{}
	fs.Var(dockerFlags, "docker-flag", "Extra flag passed verbatim to docker/podman run; may be repeated")
	fs.Usage = func() { printSubcommandUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	extra := fs.Args()
	if len(extra) == 0 {
		return errors.New("no command specified")
	}
	ct, err := a.newContainer(ctx, cf, extraRepos.values)
	if err != nil {
		return err
	}
	baseImage, err := cf.baseImage()
	if err != nil {
		return err
	}
	platform, err := cf.containerPlatform()
	if err != nil {
		return err
	}
	caches, err := resolveCaches(cacheSpecs.values, noCacheSpecs.values, *noCaches)
	if err != nil {
		return err
	}
	mounts, err := resolveMounts(mountSpecs.values)
	if err != nil {
		return err
	}
	harnessMounts, err := ct.AgentMounts(slices.Collect(maps.Values(md.HarnessMounts))...)
	if err != nil {
		return err
	}
	mounts = append(harnessMounts, mounts...)
	githubToken, err := resolveGithubToken(ctx, ct.Client, *github)
	if err != nil {
		return err
	}
	extraEnv, err := resolveEnvSpecs(envSpecs.values, os.LookupEnv)
	if err != nil {
		return err
	}
	if githubToken != "" {
		extraEnv = append(extraEnv, "GITHUB_TOKEN="+githubToken)
	}
	opts := md.StartOpts{
		BaseImage:    baseImage,
		Platform:     platform,
		Caches:       caches,
		Mounts:       mounts,
		Quiet:        true,
		ExtraEnv:     extraEnv,
		MaxCPUs:      *cpus,
		ExtraRunArgs: dockerFlags.values,
	}
	exitCode, err := runTemporaryContainer(ctx, ct, os.Stdout, os.Stderr, extra, &opts, *applyPatch)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return &exitCodeError{code: exitCode}
	}
	return nil
}

func runTemporaryContainer(ctx context.Context, c *md.Container, stdout, stderr io.Writer, command []string, opts *md.StartOpts, applyPatch bool) (int, error) {
	if len(command) == 0 {
		return 1, errors.New("no command specified")
	}
	if applyPatch && len(c.Repos) == 0 {
		return 1, errors.New("--apply-patch requires a git repository")
	}
	tmp, err := newRunContainer(c)
	if err != nil {
		return 1, err
	}
	if err := tmp.Launch(ctx, stdout, stderr, opts); err != nil {
		return 1, errors.Join(err, purgeTemporaryContainer(ctx, tmp))
	}
	if _, err := tmp.Connect(ctx, stdout, stderr, opts); err != nil {
		return 1, errors.Join(err, purgeTemporaryContainer(ctx, tmp))
	}

	exitCode, err := runTemporaryCommand(ctx, tmp, stdout, stderr, command)
	if applyPatch {
		for i := range tmp.Repos {
			if pullErr := tmp.Pull(ctx, stdout, stderr, i, nil); pullErr != nil {
				err = errors.Join(err, fmt.Errorf("applying patch for %s: %w", tmp.Repos[i].MountedPath, pullErr))
				if exitCode == 0 {
					exitCode = 1
				}
			}
		}
	}
	if cleanupErr := purgeTemporaryContainer(ctx, tmp); cleanupErr != nil {
		if exitCode != 0 {
			_, _ = fmt.Fprintf(stderr, "md: %v\n", cleanupErr)
			return exitCode, err
		}
		return 1, errors.Join(err, cleanupErr)
	}
	return exitCode, err
}

func newRunContainer(c *md.Container) (*md.Container, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, fmt.Errorf("generate temporary container suffix: %w", err)
	}
	repos := slices.Clone(c.Repos)
	name := "md-run-" + hex.EncodeToString(suffix[:])
	if len(c.Repos) > 0 {
		repoName := runContainerNameComponent(filepath.Base(c.Repos[0].MountedPath))
		name = "md-" + repoName + "-run-" + hex.EncodeToString(suffix[:])
	}
	return &md.Container{
		Client: c.Client,
		Repos:  repos,
		Name:   name,
	}, nil
}

func runTemporaryCommand(ctx context.Context, c *md.Container, stdout, stderr io.Writer, command []string) (int, error) {
	sshCommand := shellQuoteArgs(command)
	if len(c.Repos) > 0 {
		sshCommand = "cd " + shellQuote(c.Repos[0].MountedPath) + " && " + sshCommand
	}
	sshArgs := c.SSHCommand(nil, sshCommand)
	slog.DebugContext(ctx, "md", "msg", "ssh", "container", c.Name, "cmd", sshArgs)
	cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // SSH target is an md container name and command is shell-quoted.
	cmd.Env = append(os.Environ(), "LANG=C")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("run command in temporary container %s: %w", c.Name, err)
	}
	return 0, nil
}

func purgeTemporaryContainer(ctx context.Context, c *md.Container) error {
	if err := c.Purge(ctx, io.Discard, io.Discard); err != nil && err.Error() != c.Name+" not found" {
		return fmt.Errorf("cleanup temporary container %s: %w", c.Name, err)
	}
	return nil
}

func runContainerNameComponent(s string) string {
	var b strings.Builder
	lastSeparator := false
	for _, r := range s {
		if isASCIIAlnum(r) {
			b.WriteRune(r)
			lastSeparator = false
			continue
		}
		if r == '-' || r == '_' || r == '.' || r == '/' || r == '@' || r == '#' || r == ':' || r == '~' {
			if b.Len() != 0 && !lastSeparator {
				b.WriteByte('-')
				lastSeparator = true
			}
		}
	}
	name := strings.Trim(b.String(), "-_.")
	if name == "" {
		return "unnamed"
	}
	return name
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
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

// containerListEntry is the JSON representation of a container in `md list --json`.
type containerListEntry struct {
	Name      string            `json:"name"`
	State     string            `json:"state"`
	Uptime    string            `json:"uptime"`
	SSHPort   int32             `json:"ssh_port"`
	VNCPort   int32             `json:"vncPort,omitempty"`
	Display   bool              `json:"display,omitempty"`
	Tailscale bool              `json:"tailscale,omitempty"`
	FQDN      string            `json:"fqdn,omitempty"`
	Sudo      bool              `json:"sudo,omitempty"`
	USB       bool              `json:"usb,omitempty"`
	Repos     []md.Repo         `json:"repos,omitempty"`
	Stats     *containers.Stats `json:"stats,omitempty"`
}

func (a *app) cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	showStats := fs.Bool("stats", false, "Include resource usage stats (CPU, mem, net, disk) for running containers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}
	c, err := a.newClient()
	if err != nil {
		return err
	}
	cts, err := c.List(ctx)
	if err != nil {
		return err
	}

	// Batch-fetch stats for all containers in 2 docker calls.
	var allStats map[string]*containers.Stats
	if *showStats && len(cts) > 0 {
		names := make([]string, len(cts))
		for i, ct := range cts {
			names[i] = ct.Name
		}
		var statsErr error
		allStats, statsErr = c.Runtime.StatsAll(ctx, names)
		if statsErr != nil {
			slog.WarnContext(ctx, "md", "msg", "fetching container stats", "err", statsErr)
		}
	}

	if *jsonOut {
		entries := make([]containerListEntry, len(cts))
		for i, ct := range cts {
			entries[i] = containerListEntry{
				Name:      ct.Name,
				State:     ct.State,
				Uptime:    time.Since(ct.CreatedAt).Truncate(time.Second).String(),
				SSHPort:   ct.SSHPort,
				VNCPort:   ct.VNCPort,
				Display:   ct.Display,
				Tailscale: ct.Tailscale,
				Sudo:      ct.Sudo,
				USB:       ct.USB,
				Repos:     ct.Repos,
				Stats:     allStats[ct.Name],
			}
			if ct.Tailscale {
				entries[i].FQDN = ct.TailscaleFQDN(ctx)
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(cts) == 0 {
		fmt.Println("No running md containers")
		return nil
	}
	// Phase 1: compute name, repo widths, and build non-tailscale features.
	type row struct {
		name     string
		repos    string
		features string
	}
	rows := make([]row, len(cts))
	tsFQDNs := make([]string, len(cts)) // populated in parallel for tailscale containers
	nameWidth := len("Container")
	repoWidth := len("Repos")
	for i, ct := range cts {
		rows[i].name = ct.Name
		nameWidth = max(nameWidth, len(rows[i].name))
		var parts []string
		for _, r := range ct.Repos {
			parts = append(parts, filepath.Base(r.MountedPath)+":"+r.Branch)
		}
		rows[i].repos = strings.Join(parts, ", ")
		repoWidth = max(repoWidth, len(rows[i].repos))
		var feats []string
		if ct.Display {
			feats = append(feats, "display")
		}
		if ct.Tailscale {
			feats = append(feats, "tailscale")
		}
		if ct.USB {
			feats = append(feats, "usb")
		}
		rows[i].features = strings.Join(feats, ",")
	}

	// Phase 2: query Tailscale FQDNs in parallel.
	var wg sync.WaitGroup
	for i, ct := range cts {
		if ct.Tailscale && ct.State == "running" {
			wg.Go(func() {
				tsFQDNs[i] = ct.TailscaleFQDN(ctx)
			})
		}
	}
	wg.Wait()

	// Phase 3: rebuild feature strings with FQDNs and compute feature width.
	featWidth := len("Features")
	for i := range cts {
		if fqdn := tsFQDNs[i]; fqdn != "" {
			rows[i].features = "tailscale:" + fqdn
		}
		featWidth = max(featWidth, len(rows[i].features))
	}
	nameWidth += 3
	repoWidth += 3
	sepWidth := nameWidth + 1 + 10 + 1 + repoWidth + 1 + 12 + 2 + featWidth
	fmt.Printf("%-*s %-10s %-*s %12s  %-*s\n", nameWidth, "Container", "Status", repoWidth, "Repos", "Uptime", featWidth, "Features")
	fmt.Println(strings.Repeat("-", sepWidth))
	for i, ct := range cts {
		fmt.Printf("%-*s %-10s %-*s %12s  %-*s\n", nameWidth, rows[i].name, ct.State, repoWidth, rows[i].repos, time.Since(ct.CreatedAt).Truncate(time.Second), featWidth, rows[i].features)
		if s := allStats[ct.Name]; s != nil {
			if ct.State == "running" {
				fmt.Printf("  CPU: %.1f%%  Mem: %s/%s (%.1f%%)  PIDs: %d\n",
					s.CPUPerc,
					md.FormatBytes(int64(s.MemUsed)), md.FormatBytes(int64(s.MemLimit)), //nolint:gosec // MemLimit fits in int64 in practice
					s.MemPerc, s.PIDs)
				diskStr := "n/a"
				if s.DiskUsed >= 0 {
					diskStr = md.FormatBytes(s.DiskUsed)
				}
				fmt.Printf("  Net: rx=%s tx=%s  Block: r=%s w=%s  Disk: %s\n",
					md.FormatBytes(int64(s.NetRx)), md.FormatBytes(int64(s.NetTx)), //nolint:gosec // network counters fit in int64 in practice
					md.FormatBytes(int64(s.BlockRead)), md.FormatBytes(int64(s.BlockWrite)), //nolint:gosec // block counters fit in int64 in practice
					diskStr)
			} else if s.DiskUsed >= 0 {
				fmt.Printf("  Disk: %s\n", md.FormatBytes(s.DiskUsed))
			}
		}
	}
	return nil
}

func cmdSSH(args []string) error {
	if err := noArgs("ssh", args); err != nil {
		return err
	}
	return errors.New("use 'ssh md-<repo>-<branch>' directly")
}

func (a *app) cmdStop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 1); err != nil {
		return err
	}
	if name := fs.Arg(0); name != "" {
		c, err := a.newClient()
		if err != nil {
			return err
		}
		ct, err := c.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("no container named %s", name)
		}
		return ct.Stop(ctx)
	}
	ct, _, err := a.findContainerAndRepo(ctx, cf)
	if err != nil {
		return err
	}
	return ct.Stop(ctx)
}

func (a *app) cmdPurge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 1); err != nil {
		return err
	}
	// A bare container name may be passed as a positional argument for
	// repo-less containers, which have no git root to search by.
	if name := fs.Arg(0); name != "" {
		c, err := a.newClient()
		if err != nil {
			return err
		}
		ct, err := c.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("no container named %s", name)
		}
		return ct.Purge(ctx, os.Stdout, os.Stderr)
	}
	ct, _, err := a.findContainerAndRepo(ctx, cf)
	if err != nil {
		return err
	}
	return ct.Purge(ctx, os.Stdout, os.Stderr)
}

func (a *app) cmdPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	all := fs.Bool("all", false, "Operate on all repos, not just the current one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}
	ct, repoIdx, err := a.findContainerAndRepo(ctx, cf)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	printBackup := func(i int, backup string) {
		repoName := filepath.Base(ct.Repos[i].MountedPath)
		mu.Lock()
		fmt.Printf("- %s: previous state saved as git branch: %s\n", repoName, backup)
		mu.Unlock()
	}
	if !*all {
		backup, err := ct.Push(ctx, os.Stdout, os.Stderr, repoIdx)
		if err != nil {
			return err
		}
		printBackup(repoIdx, backup)
		return nil
	}
	eg, ctx2 := errgroup.WithContext(ctx)
	for i := range ct.Repos {
		eg.Go(func() error {
			backup, err := ct.Push(ctx2, os.Stdout, os.Stderr, i)
			if err != nil {
				return err
			}
			if backup != "" {
				printBackup(i, backup)
			}
			return nil
		})
	}
	return eg.Wait()
}

func (a *app) cmdPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	all := fs.Bool("all", false, "Operate on all repos, not just the current one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}
	ct, repoIdx, err := a.findContainerAndRepo(ctx, cf)
	if err != nil {
		return err
	}
	p, err := newProvider(ctx, os.Getenv("ASK_PROVIDER"), os.Getenv("ASK_MODEL"))
	if err != nil {
		slog.WarnContext(ctx, "md", "msg", "failed to initialize provider", "err", err)
	}
	if !*all {
		return ct.Pull(ctx, os.Stdout, os.Stderr, repoIdx, p)
	}
	eg, ctx2 := errgroup.WithContext(ctx)
	for i := range ct.Repos {
		eg.Go(func() error {
			return ct.Pull(ctx2, os.Stdout, os.Stderr, i, p)
		})
	}
	return eg.Wait()
}

func (a *app) cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	all := fs.Bool("all", false, "Operate on all repos, not just the current one")
	// Separate md-own flags from git passthrough args.
	// Flags defined on fs go to mdArgs; everything else (e.g. --stat,
	// --name-only) is forwarded to git diff. "--" explicitly ends md flag
	// parsing; everything after goes to git.
	var mdArgs, gitArgs []string
	for i := 0; i < len(args); i++ {
		if a := args[i]; a != "--" && strings.HasPrefix(a, "-") {
			name := strings.TrimLeft(a, "-")
			// Handle -flag=value form.
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			if f := fs.Lookup(name); f != nil {
				mdArgs = append(mdArgs, a)
				// Consume the next arg as value for non-bool flags without inline =.
				type isBool interface{ IsBoolFlag() bool }
				if _, isBoolFlag := f.Value.(isBool); !isBoolFlag && !strings.Contains(a, "=") {
					i++
					if i < len(args) {
						mdArgs = append(mdArgs, args[i])
					}
				}
				continue
			}
		}
		gitArgs = append(gitArgs, args[i:]...)
		break
	}
	if err := fs.Parse(mdArgs); err != nil {
		return err
	}
	initLogging(*verbose)
	ct, repoIdx, err := a.findContainerAndRepo(ctx, cf)
	if err != nil {
		return err
	}
	indices := []int{repoIdx}
	if *all {
		indices = make([]int, len(ct.Repos))
		for i := range ct.Repos {
			indices[i] = i
		}
	}
	for _, i := range indices {
		if *all && len(ct.Repos) > 1 {
			fmt.Printf("=== %s ===\n", filepath.Base(ct.Repos[i].GitRoot))
		}
		if err := ct.Diff(ctx, os.Stdout, os.Stderr, i, gitArgs); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) cmdFork(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fork", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	source := fs.String("source", "", "Name of the source container (default: auto-detect from repo)")
	fs.StringVar(source, "s", "", "Name of the source container (default: auto-detect from repo)")
	var display tristateBool
	fs.Var(&display, "display", "Set X11/VNC display on fork (default: inherit; use -display=false to disable)")
	var tailscale tristateBool
	fs.Var(&tailscale, "tailscale", "Set Tailscale networking on fork (default: inherit; use -tailscale=false to disable)")
	var usb tristateBool
	fs.Var(&usb, "usb", "Set USB passthrough on fork (default: inherit; use -usb=false to disable)")
	var sudoFlag tristateBool
	fs.Var(&sudoFlag, "sudo", "Set sudo access on fork (default: inherit; use -sudo=false to disable)")
	quiet := fs.Bool("q", false, "Suppress informational messages")
	noSSH := fs.Bool("no-ssh", false, "Don't SSH into the forked container after starting")
	github := fs.Bool("github", false, "Inject GitHub token into container")
	cpus := fs.Int("cpus", md.DefaultMaxCPUs(), "Max CPU cores for the container (0=no limit)")
	dockerFlags := &shellSplitSlice{}
	fs.Var(dockerFlags, "docker-flag", "Extra flag passed verbatim to docker/podman run; may be repeated")
	extraRepos := &stringSlice{}
	fs.Var(extraRepos, "extra-repo", "Additional git repository path[:branch] to map; may be repeated")
	fs.Var(extraRepos, "e", "Additional git repository path[:branch] to map; may be repeated")
	labels := &stringSlice{}
	fs.Var(labels, "label", "Set Docker container label (key=value); can be repeated")
	fs.Var(labels, "l", "Set Docker container label (key=value); can be repeated")
	fs.Usage = func() { printSubcommandUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}

	// Find the source container: by name if -source is given, otherwise
	// auto-detect from the repo like push does.
	var sourceCt *md.Container
	if *source != "" {
		c, err := a.newClient()
		if err != nil {
			return err
		}
		sourceCt, err = c.Get(ctx, *source)
		if err != nil {
			return fmt.Errorf("container %q not found", *source)
		}
	} else {
		var err error
		sourceCt, _, err = a.findContainerAndRepo(ctx, cf)
		if err != nil {
			return err
		}
	}
	githubToken, err := resolveGithubToken(ctx, sourceCt.Client, *github)
	if err != nil {
		return err
	}
	var extraEnv []string
	if githubToken != "" {
		extraEnv = append(extraEnv, "GITHUB_TOKEN="+githubToken)
	}
	mounts, err := sourceCt.AgentMounts(slices.Collect(maps.Values(md.HarnessMounts))...)
	if err != nil {
		return err
	}
	resolved, err := resolveRepoSpecs(ctx, sourceCt.Logger, extraRepos.values)
	if err != nil {
		return err
	}
	opts := md.ForkOpts{
		ExtraRepos:   resolved,
		Display:      display.resolveForkCapability(sourceCt.Display),
		Tailscale:    tailscale.resolveForkCapability(sourceCt.Tailscale),
		USB:          usb.resolveForkCapability(sourceCt.USB),
		Sudo:         sudoFlag.resolveForkCapability(sourceCt.Sudo),
		Labels:       labels.values,
		Quiet:        *quiet,
		ExtraEnv:     extraEnv,
		Mounts:       mounts,
		MaxCPUs:      *cpus,
		ExtraRunArgs: dockerFlags.values,
	}
	fork, err := sourceCt.Fork(ctx, os.Stdout, os.Stderr, &opts)
	if err != nil {
		return err
	}
	if !*quiet {
		if err := printContainerSummary(ctx, fork, nil, fmt.Sprintf("- Forked %s → %s", sourceCt.Name, fork.Name)); err != nil {
			return err
		}
	}
	if !*noSSH {
		sshArgs := fork.SSHCommand(nil, "")
		slog.DebugContext(ctx, "md", "msg", "ssh", "container", fork.Name, "cmd", sshArgs)
		cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // args are from trusted SSH config
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

func (a *app) cmdVNC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("vnc", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 1); err != nil {
		return err
	}
	var ct *md.Container
	if name := fs.Arg(0); name != "" {
		c, err := a.newClient()
		if err != nil {
			return err
		}
		ct = &md.Container{Client: c, Name: name}
	} else {
		var err error
		ct, _, err = a.findContainerAndRepo(ctx, cf)
		if err != nil {
			return err
		}
	}
	vncPort, err := ct.Runtime.HostPort(ctx, ct.Name, "5901/tcp")
	if err != nil {
		return err
	}
	if vncPort == 0 {
		return fmt.Errorf("VNC port not found for %s. Did you start it with --display?\nTo enable display, run:\n  md purge\n  md start --display", ct.Name)
	}
	vncURL := fmt.Sprintf("vnc://127.0.0.1:%d", vncPort)
	fmt.Printf("VNC connection: %s\n", vncURL)

	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", vncURL).Run() //nolint:gosec // vncURL is constructed from trusted port
	case "linux":
		if err := exec.CommandContext(ctx, "xdg-open", vncURL).Run(); err == nil { //nolint:gosec // vncURL is constructed from trusted port
			return nil
		}
		if err := exec.CommandContext(ctx, "vncviewer", fmt.Sprintf("127.0.0.1:%d", vncPort)).Run(); err == nil { //nolint:gosec // vncPort is from trusted container
			return nil
		}
		fmt.Println("\nNo VNC client found. Connect manually:")
		fmt.Println("  Address: 127.0.0.1")
		fmt.Printf("  Port: %d\n", vncPort)
		fmt.Println("\nInstall a VNC client:")
		fmt.Println("  Ubuntu/Debian: sudo apt install tigervnc-viewer")
		fmt.Println("  Fedora/RHEL: sudo dnf install tigervnc")
		fmt.Println("  Or use any remote desktop client (Remmina, RealVNC, TigerVNC, etc.)")
		return nil
	case "windows":
		if err := exec.CommandContext(ctx, "cmd", "/c", "start", "", vncURL).Run(); err == nil { //nolint:gosec // vncURL is constructed from trusted port
			return nil
		}
		fmt.Println("\nCannot launch VNC client automatically. Connect manually:")
		fmt.Println("  Address: 127.0.0.1")
		fmt.Printf("  Port: %d\n", vncPort)
		fmt.Println("\nYou can use:")
		fmt.Println("  - TigerVNC: https://tigervnc.org/")
		fmt.Println("  - RealVNC: https://www.realvnc.com/")
		return nil
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func (a *app) cmdSudoPassword(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sudo-password", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 1); err != nil {
		return err
	}
	// Accept a container name as positional arg, otherwise auto-detect.
	var ct *md.Container
	if name := fs.Arg(0); name != "" {
		c, err := a.newClient()
		if err != nil {
			return err
		}
		ct = &md.Container{Client: c, Name: name}
	} else {
		var err error
		ct, _, err = a.findContainerAndRepo(ctx, cf)
		if err != nil {
			return err
		}
	}
	password, err := ct.SudoPassword(ctx)
	if err != nil {
		return fmt.Errorf("retrieving sudo password from %s: %w", ct.Name, err)
	}
	if password == "" {
		return fmt.Errorf("no sudo password found for %s", ct.Name)
	}
	fmt.Println(password)
	return nil
}

func (a *app) cmdBuildImage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build-image", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	platformFlag := fs.String("platform", "", "Image platform: linux/amd64 or linux/arm64 (default: "+md.DefaultPlatform().String()+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}
	c, err := a.newClient()
	if err != nil {
		return err
	}
	ensureGithubToken(ctx, c)
	return c.BuildImage(ctx, os.Stdout, os.Stderr, md.Platform(*platformFlag))
}

func (a *app) cmdPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	if err := checkArgs(fs, 0); err != nil {
		return err
	}
	c, err := a.newClient()
	if err != nil {
		return err
	}
	removed, err := c.PruneImages(ctx, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Println("No unused md images to remove")
		return nil
	}
	for _, name := range removed {
		fmt.Printf("Removed %s\n", name)
	}
	return nil
}

func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("version: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		_, err := fmt.Println("md (unknown version; no build info)")
		return err
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	version := info.Main.Version
	if version == "" || version == "(devel)" {
		// No module version stamped; build from VCS info.
		rev := settings["vcs.revision"]
		if rev == "" {
			_, err := fmt.Println("md (unknown version; no VCS info)")
			return err
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		version = rev
		if settings["vcs.modified"] == "true" {
			version += "-dirty"
		}
		if t := settings["vcs.time"]; t != "" {
			version += " " + t
		}
	}
	_, err := fmt.Printf("md %s\n", version)
	return err
}

func noArgs(cmd string, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%s: unexpected arguments: %s", cmd, strings.Join(args, " "))
	}
	return nil
}

func checkArgs(fs *flag.FlagSet, maxArgs int) error {
	if fs.NArg() > maxArgs {
		return fmt.Errorf("%s: unexpected arguments: %s", fs.Name(), strings.Join(fs.Args()[maxArgs:], " "))
	}
	return nil
}

// exitCodeError is returned when a subcommand needs to exit with a specific
// non-zero code without printing an error message.
type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("exit code %d", e.code)
}

// printSubcommandUsage prints flag defaults followed by harness and cache
// reference tables.
func printSubcommandUsage(fs *flag.FlagSet) {
	w := fs.Output()
	_, _ = fmt.Fprintf(w, "Usage of %s:\n", fs.Name())
	fs.PrintDefaults()
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Harnesses:")
	harnesses := slices.Sorted(maps.Keys(md.HarnessMounts))
	for _, h := range harnesses {
		ap := md.HarnessMounts[h]
		_, _ = fmt.Fprintf(w, "  %-12s %s\n", h, ap.Description)
	}
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Well-known caches (for --cache / --no-cache):")
	names := slices.Sorted(maps.Keys(md.WellKnownCaches))
	for _, name := range names {
		desc := md.WellKnownCaches[name][0].Description
		_, _ = fmt.Fprintf(w, "  %-12s %s\n", name, desc)
	}
}

// wellKnownCacheList returns a sorted comma-separated list of well-known cache
// names for use in error messages.
func wellKnownCacheList() string {
	names := slices.Sorted(maps.Keys(md.WellKnownCaches))
	return strings.Join(names, ", ")
}

// resolveCaches builds the list of CacheMounts to bake into the image.
//
// By default all well-known caches are included (sorted by name).
// excluded names remove specific well-known caches from that default set.
// noAll disables all defaults; only caches from customSpecs are included.
// customSpecs accepts well-known names (to re-add an excluded cache when used
// with noAll) or "host:container[:ro]" custom paths.
func resolveCaches(customSpecs, excluded []string, noAll bool) ([]md.CacheMount, error) {
	result := make([]md.CacheMount, 0)

	if !noAll {
		// Validate excluded names before building the result.
		for _, n := range excluded {
			if _, ok := md.WellKnownCaches[n]; !ok {
				return nil, fmt.Errorf("unknown --no-cache name %q; valid names: %s", n, wellKnownCacheList())
			}
		}
		excl := make(map[string]struct{}, len(excluded))
		for _, n := range excluded {
			excl[n] = struct{}{}
		}
		names := make([]string, 0, len(md.WellKnownCaches))
		for k := range md.WellKnownCaches {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			if _, excluded := excl[name]; !excluded {
				result = append(result, md.WellKnownCaches[name]...)
			}
		}
	}

	// Track mount names already present to avoid duplicates from --cache.
	seen := make(map[string]struct{}, len(result))
	for _, m := range result {
		seen[m.Name] = struct{}{}
	}

	// Process --cache specs: well-known names or custom host:container[:ro].
	for _, spec := range customSpecs {
		if mounts, ok := md.WellKnownCaches[spec]; ok {
			for _, m := range mounts {
				if _, ok := seen[m.Name]; !ok {
					result = append(result, m)
					seen[m.Name] = struct{}{}
				}
			}
			continue
		}
		// Custom spec: host:container or host:container:ro.
		parts := strings.SplitN(spec, ":", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid --cache %q: use a well-known name (%s) or host:container[:ro]",
				spec, wellKnownCacheList())
		}
		cm := md.CacheMount{
			Name:          customCacheName(parts[0]),
			HostPath:      parts[0],
			ContainerPath: md.ResolveContainerPath(parts[1]),
		}
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf("invalid --cache %q: only ':ro' modifier is supported", spec)
			}
			cm.ReadOnly = true
		}
		result = append(result, cm)
	}
	return result, nil
}

// resolveMounts builds the list of runtime bind mounts.
//
// specs accepts "host:container[:ro]" paths.
func resolveMounts(specs []string) ([]md.Mount, error) {
	result := make([]md.Mount, 0, len(specs))
	for _, spec := range specs {
		parts := strings.SplitN(spec, ":", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid --mount %q: use host:container[:ro]", spec)
		}
		m := md.Mount{
			HostPath:      parts[0],
			ContainerPath: md.ResolveContainerPath(parts[1]),
		}
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf("invalid --mount %q: only ':ro' modifier is supported", spec)
			}
			m.ReadOnly = true
		}
		result = append(result, m)
	}
	return result, nil
}

func resolveEnvSpecs(specs []string, lookup func(string) (string, bool)) ([]string, error) {
	result := make([]string, 0, len(specs))
	for _, spec := range specs {
		name, value, hasValue := strings.Cut(spec, "=")
		if !validEnvName(name) {
			return nil, fmt.Errorf("invalid --env %q: environment variable names must match [A-Za-z_][A-Za-z0-9_]*", spec)
		}
		if hasValue {
			result = append(result, name+"="+value)
			continue
		}
		value, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("--env %s: host environment variable is not set", name)
		}
		result = append(result, name+"="+value)
	}
	return result, nil
}

func validEnvName(name string) bool {
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

func customCacheName(hostPath string) string {
	if hostPath == "~" || hostPath == "~/" || hostPath == `~\` {
		return "home"
	}
	base := strings.ToLower(filepath.Base(hostPath))
	var b strings.Builder
	lastHyphen := false
	for _, r := range base {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "cache"
	}
	return name
}

// tristateBool implements an optional bool flag.
//
// The zero value is unset. `-flag` sets true, `-flag=false` sets false.
type tristateBool struct {
	set   bool
	value bool
}

func (b *tristateBool) String() string {
	if b == nil || !b.set {
		return "unset"
	}
	return strconv.FormatBool(b.value)
}

func (b *tristateBool) Set(v string) error {
	value, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	b.set = true
	b.value = value
	return nil
}

func (*tristateBool) IsBoolFlag() bool { return true }

func (b *tristateBool) resolveForkCapability(source bool) bool {
	if b != nil && b.set {
		return b.value
	}
	return source
}

// stringSlice implements flag.Value for repeatable string flags.
type stringSlice struct {
	values []string
}

func (s *stringSlice) String() string {
	return strings.Join(s.values, ", ")
}

func (s *stringSlice) Set(v string) error {
	s.values = append(s.values, v)
	return nil
}

// shellSplitSlice implements flag.Value for repeatable flags whose values are
// shell-split into individual arguments. e.g. --docker-flag="--memory 4g"
// produces ["--memory", "4g"].
type shellSplitSlice struct {
	values []string
}

func (s *shellSplitSlice) String() string {
	return strings.Join(s.values, ", ")
}

func (s *shellSplitSlice) Set(v string) error {
	args, err := shellSplit(v)
	if err != nil {
		return err
	}
	s.values = append(s.values, args...)
	return nil
}

// shellSplit splits a string into words following shell quoting rules
// (single quotes, double quotes, backslash escapes).
func shellSplit(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			inWord = true
			i += 2
		case c == '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated single quote in %q", s)
			}
			inWord = true
			i++
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					cur.WriteByte(s[i+1])
					i += 2
				} else {
					cur.WriteByte(s[i])
					i++
				}
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote in %q", s)
			}
			inWord = true
			i++
		case c == ' ' || c == '\t':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

func newProvider(ctx context.Context, provider, model string) (genai.Provider, error) {
	m := genai.ProviderOptionModel(model)
	if m == "" {
		m = genai.ModelCheap
	}
	if provider != "" {
		cfg, ok := providers.All[provider]
		if !ok {
			return nil, fmt.Errorf("unknown provider %q", provider)
		}
		return cfg.Factory(ctx, m)
	}
	// Auto-discover: prefer CLI-based providers, then alphabetically.
	provs := providers.Available(ctx)
	if len(provs) == 0 {
		return nil, errors.New("no providers available")
	}
	order := append([]string{"pi", "codex", "opencode", "claudecode"}, slices.Sorted(maps.Keys(provs))...)
	for _, name := range order {
		cfg, ok := provs[name]
		if !ok {
			continue
		}
		c, err := cfg.Factory(ctx, m)
		if err != nil {
			slog.DebugContext(ctx, "md", "msg", "provider skipped", "provider", name, "error", err)
			continue
		}
		return c, nil
	}
	return nil, errors.New("no providers could be loaded")
}
