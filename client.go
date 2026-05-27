// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Package md manages isolated Docker development containers for AI coding
// agents.
//
// It provides programmatic access to create, manage, and tear down containers
// with SSH access. Containers optionally receive a full git clone of one or
// more repositories; repo-less containers are also supported for general
// agent workloads.
package md

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client holds global MD tool state (paths, image config, SSH keys).
type Client struct {
	// Paths.
	Home          string
	XDGConfigHome string
	XDGDataHome   string
	XDGStateHome  string

	// SSH key paths.
	HostKeyPath string // ~/.config/md/ssh_host_ed25519_key (generated)
	UserKeyPath string // ~/.ssh/md

	// Container runtime.
	Runtime string // "docker" or "podman"; auto-detected by New().

	// ControlMaster enables SSH ControlMaster connection multiplexing.
	// When true, SSH connections are shared via a persistent socket,
	// reducing connection overhead. Disabled by default because stale
	// sockets can cause connectivity issues that are hard to diagnose.
	ControlMaster bool

	// Tokens.
	GithubToken string // GitHub API token for Docker build secrets.
	// TailscaleAPIKey is the Tailscale API key for auth key generation and device deletion.
	//
	// It is necessary to setup ephemeral nodes. The key must be rotated every 90 days.
	//
	// See https://tailscale.com/docs/reference/tailscale-api and
	// https://tailscale.com/docs/features/ephemeral-nodes
	TailscaleAPIKey string

	// DigestCacheTTL controls how long remote image digest lookups are cached.
	// When zero, caching is disabled and the registry is queried on every start.
	DigestCacheTTL time.Duration

	// keysDir is the directory containing SSH host keys and authorized_keys
	// (~/.config/md/), used as a named Docker build context.
	keysDir string

	// env holds extra environment variables appended to subprocess
	// environments (podman, ssh, git, etc.).
	env []string

	// buildMu serializes image build operations (BuildImage, Warmup, and the
	// build step inside Launch) so concurrent callers don't race on the same
	// image tag.
	buildMu sync.Mutex

	// mu protects digestCache and imageBuildCache.
	mu sync.Mutex
	// digestCache caches remote image digest queries to avoid repeated
	// registry network round-trips. Entries expire after DigestCacheTTL.
	digestCache map[string]remoteDigestEntry
	// imageBuildCache stores the last imageBuildNeeded result so that
	// back-to-back checks (e.g. Warmup then Launch) skip redundant
	// docker inspect calls. Protected by mu; invalidated on successful build.
	imageBuildCache *imageBuildCacheEntry
}

// New creates a Client with global MD tool config and initialises SSH
// infrastructure (keys, authorized_keys, config.d include).
func New(stdout io.Writer) (*Client, error) {
	return newClient("", "", stdout)
}

// newClient is like New but allows overriding the home and runtime. When
// home is empty, os.UserHomeDir() is used and XDG_* env vars are respected.
// When home is explicit, all paths derive from it unconditionally. When rt
// is empty, detectRuntime() is called.
func newClient(home, rt string, stdout io.Writer) (*Client, error) {
	fromEnv := home == ""
	if fromEnv {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	xdgConfigHome := filepath.Join(home, ".config")
	xdgDataHome := filepath.Join(home, ".local", "share")
	xdgStateHome := filepath.Join(home, ".local", "state")
	if fromEnv {
		xdgConfigHome = envOr("XDG_CONFIG_HOME", xdgConfigHome)
		xdgDataHome = envOr("XDG_DATA_HOME", xdgDataHome)
		xdgStateHome = envOr("XDG_STATE_HOME", xdgStateHome)
	}
	if rt == "" {
		rt = detectRuntime()
	}
	c := &Client{
		Home:           home,
		XDGConfigHome:  xdgConfigHome,
		XDGDataHome:    xdgDataHome,
		XDGStateHome:   xdgStateHome,
		HostKeyPath:    filepath.Join(xdgConfigHome, "md", "ssh_host_ed25519_key"),
		UserKeyPath:    filepath.Join(home, ".ssh", "md"),
		Runtime:        rt,
		DigestCacheTTL: 12 * time.Hour,
		digestCache:    make(map[string]remoteDigestEntry),
	}
	c.keysDir = filepath.Join(c.XDGConfigHome, "md")
	if err := c.setupSSH(stdout); err != nil {
		return nil, err
	}
	return c, nil
}

// setupSSH ensures SSH keys, authorized_keys, and ~/.ssh/config.d exist.
// Called once by New(); idempotent.
func (c *Client) setupSSH(stdout io.Writer) error {
	for _, d := range []string{
		filepath.Dir(c.HostKeyPath), // ~/.config/md/
		filepath.Join(c.Home, ".ssh", "config.d"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	sshDir := filepath.Join(c.Home, ".ssh")
	if err := ensureSSHConfigInclude(stdout, sshDir); err != nil {
		return err
	}
	if err := ensureEd25519Key(stdout, c.UserKeyPath, "md-user"); err != nil {
		return err
	}
	if err := ensureEd25519Key(stdout, c.HostKeyPath, "md-host"); err != nil {
		return err
	}
	pubKey, err := os.ReadFile(c.UserKeyPath + ".pub")
	if err != nil {
		return err
	}
	authKeysPath := filepath.Join(c.keysDir, "authorized_keys")
	if existing, _ := os.ReadFile(authKeysPath); bytes.Equal(existing, pubKey) {
		return nil
	}
	return os.WriteFile(authKeysPath, pubKey, 0o600) //nolint:gosec // path is constructed from trusted config dir
}

// detectRuntime returns the container runtime to use.
// Checks for docker, then podman in PATH.
func detectRuntime() string {
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	return "docker"
}

// Container returns a Container handle for the given repos.
// It populates MountedPath on each repo from GitRoot if not already set
// (repos is mutated in place). The first repo is the primary. When called
// with no repos, the container has no associated git repository and a
// random name is generated automatically.
// It doesn't start it, it is just a reference.
func (c *Client) Container(repos ...Repo) (*Container, error) {
	// Validate repos, auto-populating MountedPath from GitRoot.
	for i := range repos {
		if err := repos[i].Validate(); err != nil {
			return nil, fmt.Errorf("repos[%d]: %w", i, err)
		}
	}
	if len(repos) == 0 {
		var buf [4]byte
		_, _ = rand.Read(buf[:])
		return &Container{
			Client: c,
			Name:   fmt.Sprintf("md-agent-%x", buf),
		}, nil
	}
	return &Container{
		Client: c,
		Repos:  repos,
		Name:   containerName(repos[0].MountName(), repos[0].Branch),
	}, nil
}

// List returns running md containers sorted by name.
func (c *Client) List(ctx context.Context) ([]*Container, error) {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "ps", "--all", "--no-trunc", "--format", "{{json .}}"})
	if err != nil {
		return nil, err
	}
	var containers []*Container
	var parseErrs []error
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		ct, err := unmarshalContainer([]byte(line))
		if err != nil {
			parseErrs = append(parseErrs, err)
			continue
		}
		if strings.HasPrefix(ct.Name, "md-") {
			ct.Client = c
			containers = append(containers, &ct)
		}
	}
	if len(containers) == 0 && len(parseErrs) > 0 {
		return nil, fmt.Errorf("failed to parse container output: %w", parseErrs[0])
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, nil
}

// Get returns a single Container by name, or an error if not found.
// Uses docker inspect for a targeted lookup.
func (c *Client) Get(ctx context.Context, name string) (*Container, error) {
	out, err := c.runCmd(ctx, "", []string{c.Runtime, "inspect", name})
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", name, err)
	}
	ct := &Container{Client: c}
	if err := fillFromInspect(ct, []byte(out)); err != nil {
		return nil, fmt.Errorf("parsing container %s: %w", name, err)
	}
	return ct, nil
}

// BuildImage builds the base Docker images locally: first md-root-local,
// then md-user-local on top of it.
func (c *Client) BuildImage(ctx context.Context, stdout, stderr io.Writer) (retErr error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	arch := runtime.GOARCH

	if c.GithubToken == "" {
		_, _ = fmt.Fprintln(stdout, "WARNING: GITHUB_TOKEN not found. Some tools (neovim, rust-analyzer, etc) might fail to install or hit rate limits.")
		_, _ = fmt.Fprintln(stdout, "Please set GITHUB_TOKEN to avoid issues:")
		_, _ = fmt.Fprintln(stdout, "  https://github.com/settings/personal-access-tokens/new?name=md-build-image&description=Token%20to%20help%20generating%20local%20docker%20images%20for%20https://github.com/caic-xyz/md")
		_, _ = fmt.Fprintln(stdout, "  export GITHUB_TOKEN=...")
	}

	// Step 1: build the root image.
	_, _ = fmt.Fprintln(stdout, "- Building root Docker image from rsc/root/Dockerfile ...")
	rootCtx, err := prepareRootBuildContext()
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(rootCtx)) }()
	rootCmd := []string{
		c.Runtime, "build",
		"--platform", "linux/" + arch,
		"-f", filepath.Join(rootCtx, "Dockerfile"),
		"-t", "md-root-local",
	}
	if c.GithubToken != "" {
		rootCmd = append(rootCmd, "--secret", "id=github_token,env=GITHUB_TOKEN")
	}
	rootCmd = append(rootCmd, rootCtx)
	if err := c.runCmdOut(ctx, "", rootCmd, stdout, stderr); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "- Root image built as 'md-root-local'.")

	// Step 2: build the user image on top of the root image.
	_, _ = fmt.Fprintln(stdout, "- Building user Docker image from rsc/user/Dockerfile ...")
	userCtx, err := prepareBuildContext()
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(userCtx)) }()
	userCmd := []string{
		c.Runtime, "build",
		"--platform", "linux/" + arch,
		"-f", filepath.Join(userCtx, "Dockerfile"),
		"--build-arg", "BASE_ROOT_IMAGE=md-root-local",
		"-t", "md-user-local",
	}
	if c.GithubToken != "" {
		userCmd = append(userCmd, "--secret", "id=github_token,env=GITHUB_TOKEN")
	}
	userCmd = append(userCmd, userCtx)
	if err := c.runCmdOut(ctx, "", userCmd, stdout, stderr); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "- User image built as 'md-user-local'.")
	c.invalidateImageBuildCache()
	// Clean up BuildKit cache (--mount=type=cache volumes from Dockerfiles).
	// These are only useful during the build itself; pruning avoids leaving
	// orphaned resources on disk.
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "builder", "prune", "-f"}); err != nil {
		_, _ = fmt.Fprintf(stdout, "- Warning: pruning build cache: %v\n", err)
	}
	return nil
}

// WarmupOpts configures base image warmup.
type WarmupOpts struct {
	// BaseImage is the full Docker image reference. When empty,
	// DefaultBaseImage+":latest" is used.
	BaseImage string
	// Caches lists host directories to COPY into the image at build time.
	Caches []CacheMount
	// Quiet suppresses informational output.
	Quiet bool
}

// Warmup ensures the base image is pulled and the user image is built,
// without starting a container. Returns true if a build was performed.
func (c *Client) Warmup(ctx context.Context, stdout, stderr io.Writer, opts *WarmupOpts) (bool, error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	baseImage := opts.BaseImage
	if baseImage == "" {
		baseImage = DefaultBaseImage + ":latest"
	}
	imageName := userImageName(baseImage, activeCacheKey(opts.Caches, c.Home))
	if !c.imageBuildNeeded(ctx, imageName, baseImage, opts.Caches) {
		if !opts.Quiet {
			_, _ = fmt.Fprintf(stdout, "- Docker image %s is up to date, skipping build.\n", imageName)
		}
		return false, nil
	}
	if err := c.buildSpecializedImage(ctx, stdout, stderr, imageName, baseImage, opts.Caches, agentContainerPaths(), opts.Quiet); err != nil {
		return false, err
	}
	c.invalidateImageBuildCache()
	return true, nil
}

// PruneImages removes md-specialized-* and md-fork-* images that are not used by any container.
// Returns the list of removed image names.
func (c *Client) PruneImages(ctx context.Context, stdout, stderr io.Writer) ([]string, error) {
	// List all md-specialized-* and md-fork-* images.
	allImages := make(map[string]struct{})
	for _, prefix := range []string{"md-specialized-*", "md-fork-*"} {
		out, err := c.runCmd(ctx, "", []string{
			c.Runtime, "images", "--format", "{{.Repository}}", "--filter", "reference=" + prefix,
		})
		if err != nil {
			return nil, fmt.Errorf("listing images: %w", err)
		}
		for name := range strings.SplitSeq(out, "\n") {
			if name != "" {
				allImages[name] = struct{}{}
			}
		}
	}
	if len(allImages) == 0 {
		return nil, nil
	}

	// Find images used by running md containers.
	containerOut, err := c.runCmd(ctx, "", []string{
		c.Runtime, "ps", "-a", "--filter", "name=^md-", "--format", "{{.Image}}",
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	inUse := make(map[string]struct{})
	if containerOut != "" {
		for img := range strings.SplitSeq(containerOut, "\n") {
			if img != "" {
				inUse[img] = struct{}{}
			}
		}
	}

	// Remove unused images.
	var removed []string
	for img := range allImages {
		if _, used := inUse[img]; used {
			continue
		}
		if _, err := c.runCmd(ctx, "", []string{c.Runtime, "rmi", img}); err != nil {
			_, _ = fmt.Fprintf(stdout, "- Warning: failed to remove %s: %v\n", img, err)
			continue
		}
		removed = append(removed, img)
	}
	sort.Strings(removed)

	// Clean up BuildKit build cache.
	if _, err := c.runCmd(ctx, "", []string{c.Runtime, "builder", "prune", "-f"}); err != nil {
		_, _ = fmt.Fprintf(stdout, "- Warning: pruning build cache: %v\n", err)
	}
	return removed, nil
}

// Harness identifies an agent harness whose config directories are mounted
// into a container.
type Harness string

// Known agent harnesses.
const (
	HarnessAmp      Harness = "amp"
	HarnessAndroid  Harness = "android"
	HarnessClaude   Harness = "claude"
	HarnessCodex    Harness = "codex"
	HarnessGemini   Harness = "gemini"
	HarnessGoose    Harness = "goose"
	HarnessKilo     Harness = "kilo"
	HarnessKimi     Harness = "kimi"
	HarnessOpencode Harness = "opencode"
	HarnessPi       Harness = "pi"
	HarnessQwen     Harness = "qwen"
)

// AgentPaths groups the relative host directory paths for one or more agent
// harnesses. Paths under HomePaths are relative to $HOME, XDGConfigPaths to
// $XDG_CONFIG_HOME (~/.config), LocalSharePaths to $XDG_DATA_HOME
// (~/.local/share), and LocalStatePaths to $XDG_STATE_HOME (~/.local/state).
type AgentPaths struct {
	// Description is a short human-readable label for the harness (e.g.
	// "Claude Code"). Displayed in settings UI.
	Description     string
	HomePaths       []string
	XDGConfigPaths  []string
	LocalSharePaths []string
	LocalStatePaths []string
}

// HarnessMounts maps each known harness to its path configuration.
var HarnessMounts = map[Harness]AgentPaths{
	HarnessAmp:      {Description: "Amp", HomePaths: []string{".amp"}, XDGConfigPaths: []string{"amp"}, LocalSharePaths: []string{"amp"}},
	HarnessAndroid:  {Description: "Android Studio", HomePaths: []string{".android"}},
	HarnessClaude:   {Description: "Claude Code", HomePaths: []string{".claude"}},
	HarnessCodex:    {Description: "Codex", HomePaths: []string{".codex"}},
	HarnessGemini:   {Description: "Gemini CLI", HomePaths: []string{".gemini"}},
	HarnessGoose:    {Description: "Goose", XDGConfigPaths: []string{"goose"}, LocalSharePaths: []string{"goose"}},
	HarnessKilo:     {Description: "Kilo Code", HomePaths: []string{".kilocode"}},
	HarnessKimi:     {Description: "Kimi", HomePaths: []string{".kimi"}},
	HarnessOpencode: {Description: "OpenCode", XDGConfigPaths: []string{"opencode"}, LocalSharePaths: []string{"opencode"}, LocalStatePaths: []string{"opencode"}},
	HarnessPi:       {Description: "Pi", HomePaths: []string{".pi"}},
	HarnessQwen:     {Description: "Qwen Code", HomePaths: []string{".qwen"}},
}

// CacheMount defines a host directory to bind-mount as a build cache inside
// the container. Well-known caches are defined in [WellKnownCaches]; custom
// mounts can be constructed directly.
type CacheMount struct {
	// Name is a human-readable identifier shown in progress output (e.g. "go-mod").
	Name string
	// Description is a short human-readable label for the cache group (e.g.
	// "Go module cache"). Displayed in settings UI.
	Description string
	// HostPath is the absolute path on the host. In [WellKnownCaches] entries
	// "~/" is used as a placeholder; call [CachesForHome] to resolve it.
	HostPath string
	// ContainerPath is the absolute path inside the container.
	ContainerPath string
	// ReadOnly mounts the directory read-only inside the container.
	ReadOnly bool
	// Shallow copies only top-level files from HostPath, ignoring
	// subdirectories. Useful for directories like ~/.android where only a few
	// files (debug.keystore, adbkey) are needed but subdirectories (avd/,
	// cache/) are large and unwanted.
	Shallow bool
}

// WellKnownCaches is the set of predefined build-tool caches, keyed by short
// name. Each name may expand to multiple [CacheMount]s (e.g. "cargo" covers
// both the registry index and git sources). HostPath values use "~/" as a
// prefix that [Container.Launch] resolves to the user's home directory at
// runtime; custom absolute paths are also accepted.
var WellKnownCaches = map[string][]CacheMount{
	"android-keys": {
		{Name: "android-keys", Description: "Android debug keystore and ADB keys", HostPath: "~/.android", ContainerPath: "/home/user/.android", Shallow: true},
	},
	"bun": {
		{Name: "bun", Description: "Bun package manager", HostPath: "~/.bun/install/cache", ContainerPath: "/home/user/.bun/install/cache"},
	},
	"cargo": {
		{Name: "cargo-registry", Description: "Rust cargo registry and git", HostPath: "~/.cargo/registry", ContainerPath: "/home/user/.cargo/registry"},
		{Name: "cargo-git", Description: "Rust cargo registry and git", HostPath: "~/.cargo/git", ContainerPath: "/home/user/.cargo/git"},
	},
	// "go-build": {
	// 	{Name: "go-build", Description: "Go build cache", HostPath: "~/.cache/go-build", ContainerPath: "/home/user/.cache/go-build"},
	// },
	"go-mod": {
		{Name: "go-mod", Description: "Go module cache", HostPath: "~/go/pkg/mod", ContainerPath: "/home/user/go/pkg/mod"},
	},
	"gradle": {
		{Name: "gradle-caches", Description: "Gradle caches and wrapper", HostPath: "~/.gradle/caches", ContainerPath: "/home/user/.gradle/caches"},
		{Name: "gradle-wrapper", Description: "Gradle caches and wrapper", HostPath: "~/.gradle/wrapper/dists", ContainerPath: "/home/user/.gradle/wrapper/dists"},
	},
	"maven": {
		{Name: "maven", Description: "Maven repository", HostPath: "~/.m2/repository", ContainerPath: "/home/user/.m2/repository"},
	},
	"npm": {
		{Name: "npm", Description: "npm cache", HostPath: "~/.npm", ContainerPath: "/home/user/.npm"},
	},
	"pip": {
		{Name: "pip", Description: "Python pip cache", HostPath: "~/.cache/pip", ContainerPath: "/home/user/.cache/pip"},
	},
	"pnpm": {
		{Name: "pnpm", Description: "pnpm store", HostPath: "~/.local/share/pnpm/store", ContainerPath: "/home/user/.local/share/pnpm/store"},
	},
	"uv": {
		{Name: "uv", Description: "UV Python package manager", HostPath: "~/.cache/uv", ContainerPath: "/home/user/.cache/uv"},
	},
}

//

var (
	reInvalid        = regexp.MustCompile(`[/@#:~]+`)
	reStripRemaining = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
	reCollapse       = regexp.MustCompile(`[-_.]{2,}`)
	reGitAt          = regexp.MustCompile(`^git@([^:]+):(.+)$`)
	reSSHGit         = regexp.MustCompile(`^ssh://git@([^/]+)/(.+)$`)
	reGitProto       = regexp.MustCompile(`^git://([^/]+)/(.+)$`)
)

// alwaysPaths are merged into every container's mount set automatically.
// Callers do not need to include these; Client methods add them internally.
var alwaysPaths = AgentPaths{
	XDGConfigPaths: []string{"agents", "md"},
}

// mergePaths concatenates a slice of AgentPaths into one, prepending alwaysPaths.
func mergePaths(paths []AgentPaths) AgentPaths {
	result := alwaysPaths
	for _, p := range paths {
		result.HomePaths = append(result.HomePaths, p.HomePaths...)
		result.XDGConfigPaths = append(result.XDGConfigPaths, p.XDGConfigPaths...)
		result.LocalSharePaths = append(result.LocalSharePaths, p.LocalSharePaths...)
		result.LocalStatePaths = append(result.LocalStatePaths, p.LocalStatePaths...)
	}
	return result
}

// agentContainerPaths returns the container-side mount target paths for all
// agent config mounts. These are the -v targets that must be pre-created with
// user ownership in the Docker image before docker run creates them as root.
func agentContainerPaths() []string {
	all := alwaysPaths
	for _, p := range HarnessMounts {
		all.HomePaths = append(all.HomePaths, p.HomePaths...)
		all.XDGConfigPaths = append(all.XDGConfigPaths, p.XDGConfigPaths...)
		all.LocalSharePaths = append(all.LocalSharePaths, p.LocalSharePaths...)
		all.LocalStatePaths = append(all.LocalStatePaths, p.LocalStatePaths...)
	}
	paths := make([]string, 0, len(all.HomePaths)+len(all.XDGConfigPaths)+len(all.LocalSharePaths)+len(all.LocalStatePaths))
	for _, p := range all.HomePaths {
		paths = append(paths, "/home/user/"+p)
	}
	for _, p := range all.XDGConfigPaths {
		paths = append(paths, "/home/user/.config/"+p)
	}
	for _, p := range all.LocalSharePaths {
		paths = append(paths, "/home/user/.local/share/"+p)
	}
	for _, p := range all.LocalStatePaths {
		paths = append(paths, "/home/user/.local/state/"+p)
	}
	return paths
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// SanitizeDockerName sanitizes a string for use in a Docker container name.
//
// Docker container names must match [a-zA-Z0-9][a-zA-Z0-9_.-].
func SanitizeDockerName(name string) string {
	s := reInvalid.ReplaceAllString(name, "-")
	s = reStripRemaining.ReplaceAllString(s, "")
	s = reCollapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	if s == "" {
		return "unnamed"
	}
	return s
}

// containerName returns the container name for a repo and branch.
func containerName(repoName, branchName string) string {
	return "md-" + SanitizeDockerName(repoName) + "-" + SanitizeDockerName(branchName)
}
