// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// md (my devenv) manages isolated Docker development containers for AI coding
// agents.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
)

// runtimeOverride is set by --runtime and applied in newClient/cmdList.
var runtimeOverride string

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

func mainImpl() error {
	// Pre-parse to support flags before the subcommand (e.g. "md -v start").
	pre := flag.NewFlagSet("md", flag.ContinueOnError)
	preVerbose := addVerboseFlag(pre)
	preRuntime := pre.String("runtime", "", "Container runtime: docker or podman (default: auto-detect)")
	// Ignore errors: unknown flags here are subcommand flags, parsed later.
	_ = pre.Parse(os.Args[1:])
	initLogging(*preVerbose)
	runtimeOverride = *preRuntime
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
	case "start":
		return cmdStart(ctx, args)
	case "run":
		return cmdRun(ctx, args)
	case "list":
		return cmdList(ctx, args)
	case "ssh":
		return cmdSSH(args)
	case "kill":
		return cmdKill(ctx, args)
	case "push":
		return cmdPush(ctx, args)
	case "pull":
		return cmdPull(ctx, args)
	case "diff":
		return cmdDiff(ctx, args)
	case "vnc":
		return cmdVNC(ctx, args)
	case "build-image":
		return cmdBuildImage(ctx, args)
	case "version":
		return cmdVersion(args)
	case "help", "-h", "-help", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "md (my devenv): local development environment with git clone")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Global flags:")
	fmt.Fprintln(os.Stderr, "  -v, -verbose       Enable debug logging")
	fmt.Fprintln(os.Stderr, "  --runtime <name>   Container runtime: docker or podman (default: auto-detect)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  start       Pull base image, rebuild if needed, start container, open shell")
	fmt.Fprintln(os.Stderr, "  run <cmd>   Start a temporary container, run a command, then clean up")
	fmt.Fprintln(os.Stderr, "  list        List running md containers")
	fmt.Fprintln(os.Stderr, "  kill        Stop and remove the container")
	fmt.Fprintln(os.Stderr, "  push        Force-push current repo state into the running container")
	fmt.Fprintln(os.Stderr, "  pull        Pull changes from container back to local branch")
	fmt.Fprintln(os.Stderr, "  diff        Show differences between base and current changes")
	fmt.Fprintln(os.Stderr, "  vnc         Open VNC connection to the container")
	fmt.Fprintln(os.Stderr, "  build-image Build the base Docker image locally")
	fmt.Fprintln(os.Stderr, "  version     Print version information")
}

func newClient() (*md.Client, error) {
	c, err := md.New()
	if err != nil {
		return nil, err
	}
	if runtimeOverride != "" {
		c.Runtime = runtimeOverride
	}
	c.GithubToken = os.Getenv("GITHUB_TOKEN")
	c.TailscaleAPIKey = os.Getenv("TAILSCALE_API_KEY")
	if err := c.Prepare(); err != nil {
		return nil, err
	}
	return c, nil
}

// containerFlags holds the common flags for commands that target a container.
type containerFlags struct {
	image  *string
	tag    *string
	branch *string
	repo   *string
}

// addContainerFlags registers -b/-branch and -repo on the given FlagSet.
// When image is true, --image and --tag are also registered.
func addContainerFlags(fs *flag.FlagSet, image bool) *containerFlags {
	cf := &containerFlags{}
	if image {
		cf.image = fs.String("image", "", "Full base Docker image (default: "+md.DefaultBaseImage+":latest)")
		cf.tag = fs.String("tag", "", "Tag for the default base image ("+md.DefaultBaseImage+":<tag>)")
	}
	cf.branch = fs.String("branch", "", "Branch to use (default: current branch)")
	fs.StringVar(cf.branch, "b", "", "Branch to use (default: current branch)")
	cf.repo = fs.String("repo", "", "Path to git repository (default: current directory)")
	fs.StringVar(cf.repo, "r", "", "Path to git repository (default: current directory)")
	return cf
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

func newContainer(ctx context.Context, cf *containerFlags) (*md.Container, error) {
	c, err := newClient()
	if err != nil {
		return nil, err
	}
	repo := ""
	if cf.repo != nil && *cf.repo != "" {
		repo = *cf.repo
	} else {
		repo, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	gitRoot, err := gitutil.RootDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(gitRoot); err != nil {
		return nil, err
	}
	var branch string
	if cf.branch != nil && *cf.branch != "" {
		branch = *cf.branch
	} else {
		branch, err = gitutil.CurrentBranch(ctx, gitRoot)
		if err != nil {
			return nil, err
		}
	}
	return c.Container(gitRoot, branch), nil
}

func cmdStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	display := fs.Bool("display", false, "Enable X11/VNC display")
	fs.BoolVar(display, "d", false, "Enable X11/VNC display")
	tailscale := fs.Bool("tailscale", false, "Enable Tailscale networking")
	usb := fs.Bool("usb", false, "Pass through USB devices (/dev/bus/usb)")
	cf := addContainerFlags(fs, true)
	noSSH := fs.Bool("no-ssh", false, "Don't SSH into the container after starting")
	quiet := fs.Bool("q", false, "Suppress informational messages")
	labels := &stringSlice{}
	fs.Var(labels, "label", "Set Docker container label (key=value); can be repeated")
	fs.Var(labels, "l", "Set Docker container label (key=value); can be repeated")
	cacheSpecs := &stringSlice{}
	fs.Var(cacheSpecs, "cache", "Add a cache: well-known name ("+wellKnownCacheList()+") or host:container[:ro]; may be repeated")
	noCacheSpecs := &stringSlice{}
	fs.Var(noCacheSpecs, "no-cache", "Exclude a default well-known cache by name; may be repeated")
	noCaches := fs.Bool("no-caches", false, "Disable all default caches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)

	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	baseImage, err := cf.baseImage()
	if err != nil {
		return err
	}
	caches, err := resolveCaches(cacheSpecs.values, noCacheSpecs.values, *noCaches)
	if err != nil {
		return err
	}
	opts := md.StartOpts{
		BaseImage:        baseImage,
		Display:          *display,
		Tailscale:        *tailscale,
		USB:              *usb,
		TailscaleAuthKey: os.Getenv("TAILSCALE_AUTHKEY"),
		Caches:           caches,
		Labels:           labels.values,
		Quiet:            *quiet,
	}
	result, err := ct.Start(ctx, &opts)
	if err != nil {
		return err
	}
	if !*quiet {
		printStartSummary(ct, result)
	}
	if !*noSSH {
		sshArgs := ct.SSHCommand(ct.Name)
		cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

func printStartSummary(ct *md.Container, r *md.StartResult) {
	fmt.Println("- Cool facts:")
	fmt.Println("  > Remote access:")
	fmt.Printf("  >  SSH: `ssh %s`\n", ct.Name)
	if r.VNCPort != "" {
		fmt.Printf("  >  VNC: connect to localhost:%s with a VNC client or: `md vnc`\n", r.VNCPort)
	} else {
		fmt.Println("  >  Next time pass --display to have a virtual display")
	}
	if r.TailscaleFQDN != "" {
		fmt.Printf("  >  Tailscale FQDN: %s\n", r.TailscaleFQDN)
	}
	if r.TailscaleAuthURL != "" {
		fmt.Printf("  >  Tailscale auth: %s\n", r.TailscaleAuthURL)
	}
	fmt.Printf("  > Host branch '%s' is mapped in the container as 'base'\n", ct.Branch)
	fmt.Println("  > See changes (in container): `git diff base`")
	fmt.Println("  > See changes    (on host)  : `md diff`")
	fmt.Println("  > Kill container (on host)  : `md kill`")
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, true)
	cacheSpecs := &stringSlice{}
	fs.Var(cacheSpecs, "cache", "Add a cache: well-known name ("+wellKnownCacheList()+") or host:container[:ro]; may be repeated")
	noCacheSpecs := &stringSlice{}
	fs.Var(noCacheSpecs, "no-cache", "Exclude a default well-known cache by name; may be repeated")
	noCaches := fs.Bool("no-caches", false, "Disable all default caches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	extra := fs.Args()
	if len(extra) == 0 {
		return errors.New("no command specified")
	}
	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	baseImage, err := cf.baseImage()
	if err != nil {
		return err
	}
	caches, err := resolveCaches(cacheSpecs.values, noCacheSpecs.values, *noCaches)
	if err != nil {
		return err
	}
	exitCode, err := ct.Run(ctx, baseImage, extra, caches)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return &exitCodeError{code: exitCode}
	}
	return nil
}

// containerListEntry is the JSON representation of a container in `md list --json`.
type containerListEntry struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Uptime    string `json:"uptime"`
	Display   bool   `json:"display,omitempty"`
	Tailscale bool   `json:"tailscale,omitempty"`
	FQDN      string `json:"fqdn,omitempty"`
	USB       bool   `json:"usb,omitempty"`
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	jsonOut := fs.Bool("json", false, "Output in JSON format")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	c, err := md.New()
	if err != nil {
		return err
	}
	if runtimeOverride != "" {
		c.Runtime = runtimeOverride
	}
	containers, err := c.List(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		entries := make([]containerListEntry, len(containers))
		for i, ct := range containers {
			entries[i] = containerListEntry{
				Name:      ct.Name,
				State:     ct.State,
				Uptime:    time.Since(ct.CreatedAt).Truncate(time.Second).String(),
				Display:   ct.Display,
				Tailscale: ct.Tailscale,
				USB:       ct.USB,
			}
			if ct.Tailscale {
				entries[i].FQDN = ct.TailscaleFQDN(ctx)
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(containers) == 0 {
		fmt.Println("No running md containers")
		return nil
	}
	fmt.Printf("%-30s %-10s %-12s %s\n", "Container", "Status", "Uptime", "Features")
	fmt.Println(strings.Repeat("-", 80))
	for _, ct := range containers {
		var features []string
		if ct.Display {
			features = append(features, "display")
		}
		if ct.Tailscale {
			if fqdn := ct.TailscaleFQDN(ctx); fqdn != "" {
				features = append(features, "tailscale:"+fqdn)
			} else {
				features = append(features, "tailscale")
			}
		}
		if ct.USB {
			features = append(features, "usb")
		}
		fmt.Printf("%-30s %-10s %-12s %s\n", ct.Name, ct.State, time.Since(ct.CreatedAt).Truncate(time.Second), strings.Join(features, ","))
	}
	return nil
}

func cmdSSH(args []string) error {
	if err := noArgs("ssh", args); err != nil {
		return err
	}
	return errors.New("use 'ssh md-<repo>-<branch>' directly")
}

func cmdKill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	return ct.Kill(ctx)
}

func cmdPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	return ct.Push(ctx)
}

func cmdPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	return ct.Pull(ctx, os.Getenv("ASK_PROVIDER"), os.Getenv("ASK_MODEL"))
}

func cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
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
	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	return ct.Diff(ctx, os.Stdout, os.Stderr, gitArgs)
}

func cmdVNC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("vnc", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	cf := addContainerFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	ct, err := newContainer(ctx, cf)
	if err != nil {
		return err
	}
	vncPort, err := ct.GetHostPort(ctx, "5901/tcp")
	if err != nil {
		return err
	}
	if vncPort == "" {
		return fmt.Errorf("VNC port not found for %s. Did you start it with --display?\nTo enable display, run:\n  md kill\n  md start --display", ct.Name)
	}
	vncURL := "vnc://127.0.0.1:" + vncPort
	fmt.Printf("VNC connection: %s\n", vncURL)

	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", vncURL).Run()
	case "linux":
		if err := exec.Command("xdg-open", vncURL).Run(); err == nil {
			return nil
		}
		if err := exec.Command("vncviewer", "127.0.0.1:"+vncPort).Run(); err == nil {
			return nil
		}
		fmt.Println("\nNo VNC client found. Connect manually:")
		fmt.Println("  Address: 127.0.0.1")
		fmt.Printf("  Port: %s\n", vncPort)
		fmt.Println("\nInstall a VNC client:")
		fmt.Println("  Ubuntu/Debian: sudo apt install tigervnc-viewer")
		fmt.Println("  Fedora/RHEL: sudo dnf install tigervnc")
		fmt.Println("  Or use any remote desktop client (Remmina, RealVNC, TigerVNC, etc.)")
		return nil
	case "windows":
		return exec.Command("cmd", "/c", "start", vncURL).Run()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func cmdBuildImage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build-image", flag.ExitOnError)
	verbose := addVerboseFlag(fs)
	serialSetup := fs.Bool("serial-setup", false, "Run setup steps serially instead of in parallel")
	if err := fs.Parse(args); err != nil {
		return err
	}
	initLogging(*verbose)
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.BuildImage(ctx, *serialSetup)
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

// exitCodeError is returned when a subcommand needs to exit with a specific
// non-zero code without printing an error message.
type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("exit code %d", e.code)
}

// wellKnownCacheList returns a sorted comma-separated list of well-known cache
// names for use in flag help text.
func wellKnownCacheList() string {
	names := make([]string, 0, len(md.WellKnownCaches))
	for k := range md.WellKnownCaches {
		names = append(names, k)
	}
	sort.Strings(names)
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
			Name:          filepath.Base(parts[0]),
			HostPath:      parts[0],
			ContainerPath: parts[1],
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
