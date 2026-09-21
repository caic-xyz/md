// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// End-to-end container lifecycle smoke tests.

//go:build smoke

package md

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/md/containers"
)

func newSmokeClient(t *testing.T, rt string) *Client {
	tmp := t.TempDir()
	tmpHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(tmpHome, 0o700); err != nil {
		t.Fatalf("create home: %v", err)
	}
	cfgDir := filepath.Join(tmpHome, ".config", "containers")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("create containers config dir: %v", err)
	}
	storageConf := "[storage]\n"
	storageConf += "driver = \"overlay\"\n"
	storageConf += "graphroot = \"" + filepath.ToSlash(tmpHome) + "/.local/share/containers/storage\"\n"
	storageConf += "runroot = \"" + filepath.ToSlash(tmp) + "/runroot\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "storage.conf"), []byte(storageConf), 0o600); err != nil {
		t.Fatalf("write storage.conf: %v", err)
	}

	logger := testLogger(t)
	clientEnv := []string{
		"HOME=" + tmpHome,
		"GIT_SSH_COMMAND=ssh -F " + filepath.Join(tmpHome, ".ssh", "config"),
		"XDG_CONFIG_HOME=" + filepath.Join(tmpHome, ".config"),
	}
	client, err := newClient(tmpHome, logger, testRuntime(t, rt, logger, clientEnv), io.Discard)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	client.env = clientEnv

	// podman system reset cleans up overlay storage before t.TempDir removal,
	// avoiding permission errors. Removing a large image can exceed 30 seconds
	// on a loaded CI runner.
	if rt == "podman" {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
			defer cancel()
			if _, err := client.Runtime.Run(ctx, "", "system", "reset", "-f"); err != nil {
				t.Errorf("podman system reset cleanup: %v", err)
			}
		})
	}
	return client
}

// hasImage checks whether a container image exists in the local store.
func hasImage(ctx context.Context, c *Client, name string) bool {
	_, err := c.Runtime.Run(ctx, "", "image", "inspect", "--format", "{{.Id}}", name)
	return err == nil
}

func removeSmokeContainerIfPresent(t testing.TB, ctx context.Context, c *Client, name string) {
	out, err := c.Runtime.Run(ctx, "", "ps", "-a", "--format", "{{.Names}}", "--filter", "name=^"+name+"$")
	if err != nil {
		t.Errorf("list container %s before cleanup: %v", name, err)
		return
	}
	if !slices.Contains(strings.Fields(out), name) {
		return
	}
	if _, err := c.Runtime.Run(ctx, "", "rm", "-f", "-v", name); err != nil {
		t.Errorf("remove container %s: %v", name, err)
	}
}

func removeSmokeImageIfPresent(t testing.TB, ctx context.Context, c *Client, name string) {
	if !hasImage(ctx, c, name) {
		return
	}
	if _, err := c.Runtime.Run(ctx, "", "rmi", "-f", name); err != nil {
		t.Errorf("remove image %s: %v", name, err)
	}
}

// ensureImages ensures md-root-local and md-user-local exist. When they don't
// and -short is passed, falls back to the remote default image instead of
// building. Returns the base image to use.
func ensureImages(t *testing.T, ctx context.Context, c *Client) string {
	if hasImage(ctx, c, "md-root-local") && hasImage(ctx, c, "md-user-local") {
		t.Log("local images already present, skipping build")
		return "md-user-local"
	}
	if testing.Short() {
		t.Log("short mode: using remote image " + DefaultBaseImage)
		return DefaultBaseImage + ":latest"
	}
	t.Log("building local images (md-root-local → md-user-local) ...")
	if err := c.BuildImage(ctx, io.Discard, io.Discard, PlatformDefault); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	t.Log("images built successfully")
	return "md-user-local"
}

// prebuildSpecializedImage builds the specialized image so that subsequent
// subtests can reuse it without racing on the build.
func prebuildSpecializedImage(t *testing.T, ctx context.Context, c *Client, baseImage string, caches []CacheMount) {
	ct, err := c.Container()
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	ct.Name = "md-smoke-prebuild"
	opts := &StartOpts{BaseImage: baseImage, Quiet: true, Caches: caches}
	if _, err := ct.ensureImage(ctx, io.Discard, io.Discard, baseImage, opts.Platform, opts.Caches, true); err != nil {
		t.Fatalf("prebuild specialized image: %v", err)
	}
}

// launchSmokeContainer creates a Container with the given name suffix and
// calls Launch+Connect. Returns the live container (caller must Purge via
// t.Cleanup).
func launchSmokeContainer(t *testing.T, ctx context.Context, c *Client, baseImage, nameSuffix string, sudo bool, caches ...CacheMount) *Container {
	if sudo && c.Runtime.IsRootless() {
		t.Skip("skipping: sudo is not supported with rootless podman")
	}
	ct, err := c.Container()
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	ct.Name = "md-smoke-" + nameSuffix

	removeSmokeContainerIfPresent(t, ctx, c, ct.Name)

	opts := &StartOpts{
		BaseImage: baseImage,
		Sudo:      sudo,
		Quiet:     true,
		Caches:    caches,
	}

	t.Logf("launching container %s ...", ct.Name)
	if err := ct.Launch(ctx, os.Stdout, os.Stderr, opts); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := ct.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
			t.Logf("cleanup %s: %v", ct.Name, err)
		}
	})

	if _, err := ct.Connect(ctx, io.Discard, io.Discard, opts); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return ct
}

// smokeForeignBaseDockerfile builds the reference Debian-family base image used
// by the foreign_base subtests. It carries only what the md startup contract
// needs beyond sshd itself: git, which md's post-SSH provisioning drives. It
// deliberately has none of the md-specific packages (no Xvnc, no tailscaled,
// no DBus, no sudo) and no `user` account, so start.sh has to provision the
// account and tolerate the subsystems md did not request.
const smokeForeignBaseDockerfile = `FROM debian:stable-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-server \
 && rm -rf /var/lib/apt/lists/*
`

// ensureSmokeForeignBase builds (or reuses) the foreign base image. The tag
// carries a hash of the Dockerfile so editing the fixture rebuilds it instead of
// silently reusing a stale image. The build is small enough to also run with
// -short, which only skips the multi-gigabyte md-root/md-user builds.
func ensureSmokeForeignBase(t *testing.T, ctx context.Context, c *Client) string {
	t.Helper()
	sum := sha256.Sum256([]byte(smokeForeignBaseDockerfile))
	image := fmt.Sprintf("md-smoke-debian-slim-%x", sum[:4])
	if hasImage(ctx, c, image) {
		t.Log("reusing foreign base image " + image)
		return image
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(smokeForeignBaseDockerfile), 0o600); err != nil {
		t.Fatalf("writing foreign base Dockerfile: %v", err)
	}
	t.Log("building foreign base image " + image + " ...")
	if out, err := c.Runtime.Run(ctx, dir, "build", "-t", image, "."); err != nil {
		t.Fatalf("building foreign base image: %v\n%s", err, out)
	}
	return image
}

// newSmokeContainer creates a named container without launching it, removing a
// leftover container with the same name and registering cleanup.
func newSmokeContainer(t *testing.T, ctx context.Context, c *Client, name string) *Container {
	t.Helper()
	ct, err := c.Container()
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	ct.Name = name
	removeSmokeContainerIfPresent(t, ctx, c, name)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := ct.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
			t.Logf("cleanup %s: %v", name, err)
		}
	})
	return ct
}

// waitForSmokeContainerExit waits for a container to stop and returns its exit
// code and combined logs. start.sh answers a missing capability by exiting
// nonzero with the capability and the requesting option on stderr, and md would
// otherwise wait out its two-minute SSH timeout for a container that is already
// gone.
func waitForSmokeContainerExit(t *testing.T, ctx context.Context, c *Client, name string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := c.Runtime.Run(ctx, "", "inspect", "--format", "{{.State.Running}}", name)
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if strings.TrimSpace(out) == "false" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s is still running", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
	out, err := c.Runtime.Run(ctx, "", "inspect", "--format", "{{.State.ExitCode}}", name)
	if err != nil {
		t.Fatalf("inspect exit code of %s: %v", name, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parsing exit code %q of %s: %v", out, name, err)
	}
	// Both streams are needed: start.sh logs to stdout and writes capability
	// errors to stderr.
	var logs strings.Builder
	if err := c.Runtime.RunOut(ctx, "", &logs, &logs, "logs", name); err != nil {
		t.Fatalf("logs of %s: %v", name, err)
	}
	return code, logs.String()
}

func launchSmokeRepoContainer(t *testing.T, ctx context.Context, c *Client, baseImage string, caches []CacheMount, repo *Repo) *Container {
	ct, err := c.Container(*repo)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}

	removeSmokeContainerIfPresent(t, ctx, c, ct.Name)

	opts := &StartOpts{
		BaseImage: baseImage,
		Quiet:     true,
		Caches:    caches,
	}

	t.Logf("launching repo container %s ...", ct.Name)
	var stdout, stderr strings.Builder
	if err := ct.Launch(ctx, &stdout, &stderr, opts); err != nil {
		t.Fatalf("Launch: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := ct.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
			t.Logf("cleanup %s: %v", ct.Name, err)
		}
	})

	if _, err := ct.Connect(ctx, &stdout, &stderr, opts); err != nil {
		t.Fatalf("Connect: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return ct
}

func createSmokeGitRepo(t *testing.T, defaultBranch, workBranch string, unpushedWorkCommit bool) string {
	return createSmokeGitRepoWithRemote(t, "origin", defaultBranch, workBranch, unpushedWorkCommit)
}

func createSmokeGitRepoWithRemote(t *testing.T, remoteName, defaultBranch, workBranch string, unpushedWorkCommit bool) string {
	ctx := t.Context()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "smoke-repo")
	remote := filepath.Join(tmp, remoteName+".git")

	runSmokeGit(t, ctx, "", "init", "-q", "--bare", remote)
	runSmokeGit(t, ctx, "", "init", "-q", "--initial-branch="+defaultBranch, repo)
	runSmokeGit(t, ctx, repo, "config", "user.name", "Smoke Test")
	runSmokeGit(t, ctx, repo, "config", "user.email", "smoke@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runSmokeGit(t, ctx, repo, "add", ".")
	runSmokeGit(t, ctx, repo, "commit", "-q", "-m", "initial")
	runSmokeGit(t, ctx, repo, "remote", "add", remoteName, remote)
	runSmokeGit(t, ctx, repo, "push", "-q", "-u", remoteName, defaultBranch)
	runSmokeGit(t, ctx, remote, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
	runSmokeGit(t, ctx, repo, "remote", "set-head", remoteName, "-a")
	if workBranch != defaultBranch {
		runSmokeGit(t, ctx, repo, "checkout", "-q", "-b", workBranch)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("remote "+workBranch+"\n"), 0o600); err != nil {
			t.Fatalf("write work branch README.md: %v", err)
		}
		runSmokeGit(t, ctx, repo, "commit", "-q", "-am", "remote "+workBranch)
		runSmokeGit(t, ctx, repo, "push", "-q", "-u", remoteName, workBranch)
	}
	if unpushedWorkCommit {
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("local "+workBranch+"\n"), 0o600); err != nil {
			t.Fatalf("write unpushed README.md: %v", err)
		}
		runSmokeGit(t, ctx, repo, "commit", "-q", "-am", "local "+workBranch)
	}
	return repo
}

func runSmokeGit(t *testing.T, ctx context.Context, wd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are test-controlled.
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "LANG=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runSmokeMD(t *testing.T, c *Client, args ...string) string {
	ctx := t.Context()
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	goArgs := append([]string{"run", "./cmd/md", "--runtime", c.Runtime.Name()}, args...)
	cmd := exec.CommandContext(ctx, "go", goArgs...) //nolint:gosec // args are test-controlled.
	cmd.Dir = repoRoot
	overrides := append([]string(nil), c.env...)
	overrides = append(overrides, "LANG=C")
	overrides = append(overrides, smokeGoEnv(t)...)
	cmd.Env = containers.EnvWithOverrides(os.Environ(), overrides)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("md %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func smokeGoEnv(t *testing.T) []string {
	cmd := exec.CommandContext(t.Context(), "go", "env", "GOCACHE", "GOMODCACHE", "GOPATH")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	keys := []string{"GOCACHE", "GOMODCACHE", "GOPATH"}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(keys) {
		t.Fatalf("go env returned %d lines, want %d:\n%s", len(lines), len(keys), out)
	}
	env := make([]string, 0, len(keys))
	for i, value := range lines {
		if value != "" {
			env = append(env, keys[i]+"="+value)
		}
	}
	return env
}

func runSmokeContainerGit(t *testing.T, ct *Container, repoPath string, args ...string) string {
	gitArgs := append([]string{"git", "-C", repoPath}, args...)
	out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, shellQuoteArgs(gitArgs)))
	if err != nil {
		t.Fatalf("container git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func assertSmokeContainerGitRef(t *testing.T, ct *Container, repoPath, ref, want string) {
	got := runSmokeContainerGit(t, ct, repoPath, "rev-parse", "--verify", ref)
	if got != want {
		t.Fatalf("container %s = %q, want %q", ref, got, want)
	}
}

func assertSmokeContainerGitRefMissing(t *testing.T, ct *Container, repoPath, ref string) {
	gitArgs := []string{"git", "-C", repoPath, "rev-parse", "--verify", ref}
	out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, shellQuoteArgs(gitArgs)))
	if err == nil {
		t.Fatalf("container %s exists unexpectedly at %q", ref, strings.TrimSpace(out))
	}
}

func assertSmokeContainerNoDiff(t *testing.T, ct *Container, repoIdx int) {
	var stdout, stderr strings.Builder
	err := ct.Diff(t.Context(), &stdout, &stderr, repoIdx, &DiffOpts{Args: []string{"--exit-code"}})
	if err != nil {
		t.Fatalf("container diff since synchronization is not empty: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("container diff since synchronization produced output:\n%s", stdout.String())
	}
}

// TestSmoke verifies end-to-end: build images, start containers, confirm sudo
// and nested podman where supported, pull from registries, and exercise the
// container lifecycle. Runs under each available container runtime.
func TestSmoke(t *testing.T) {
	t.Parallel()
	for _, rt := range []string{"docker", "podman"} {
		t.Run(rt, func(t *testing.T) {
			if _, err := exec.LookPath(rt); err != nil {
				t.Skipf("skipping: %s not in PATH", rt)
			}
			t.Parallel()

			client := newSmokeClient(t, rt)

			rootlessRuntime := client.Runtime.IsRootless()

			// Rootless Podman adds --userns=keep-id, which puts the inner
			// container in a user namespace. Nested newuidmap then fails
			// with EPERM — user namespace stacking is not supported.
			// Rootful Docker and rootful Podman do not have this limitation.
			// Error: "newuidmap: write to uid_map failed: Operation not permitted"
			nestedOK := !rootlessRuntime

			// Fetch md-user upfront so all subtests can reuse it.
			baseImage := ensureImages(t, t.Context(), client)

			// Rootless Podman can spend over a minute preparing the ID mapping for
			// each distinct large image. Include the cache fixture in the shared
			// specialized image so cache coverage does not require a second image.
			var sharedCaches []CacheMount
			var smokeCacheSource string
			if rootlessRuntime {
				smokeCacheSource = filepath.Join(t.TempDir(), "smoke-cache")
				if err := os.Mkdir(smokeCacheSource, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(smokeCacheSource, "hello.txt"), []byte("cache-works"), 0o600); err != nil {
					t.Fatal(err)
				}
				sharedCaches = []CacheMount{{
					Name:          "smoke-cache",
					HostPath:      smokeCacheSource,
					ContainerPath: "/home/user/.cache/smoke",
				}}
			}

			// Pre-build the specialized image once so subtests reuse it.
			prebuildSpecializedImage(t, t.Context(), client, baseImage, sharedCaches)

			// Serialized group: these subtests share the same
			// specialized image, so running them sequentially avoids
			// redundant image-build checks.
			t.Run("serialized", func(t *testing.T) {
				t.Run("launch", func(t *testing.T) {
					ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-launch", false, sharedCaches...)

					t.Run("sudo", func(t *testing.T) {
						ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-sudo", true, sharedCaches...)
						out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "echo '"+ct.sudoPassword+"' | sudo -S whoami"))
						if err != nil {
							t.Fatalf("sudo whoami: %v", err)
						}
						if got := strings.TrimSpace(out); got != "root" {
							t.Fatalf("sudo whoami expected 'root', got %q", got)
						}
						t.Log("sudo works inside the container")

						t.Run("fork_removes_sudo", func(t *testing.T) {
							cleanupImage := "md-fork-" + ct.Name
							t.Cleanup(func() {
								cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
								defer cancel()
								removeSmokeImageIfPresent(t, cleanupCtx, client, cleanupImage)
							})

							var forkStdout, forkStderr strings.Builder
							forkRepos := make([]ForkRepo, len(ct.Repos))
							for i, r := range ct.Repos {
								forkRepos[i] = ForkRepo{GitRoot: r.GitRoot, SourceBranches: r.Branches, DestPrimary: r.Branches[0] + "-0"}
							}
							fork, err := ct.Fork(t.Context(), &forkStdout, &forkStderr, &ForkOpts{
								Repos:   forkRepos,
								Quiet:   true,
								Sudo:    false,
								MaxCPUs: DefaultMaxCPUs(),
							})
							if err != nil {
								t.Fatalf("Fork without sudo: %v\nstdout:\n%s\nstderr:\n%s", err, forkStdout.String(), forkStderr.String())
							}
							t.Cleanup(func() {
								cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
								defer cancel()
								if err := fork.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
									t.Logf("cleanup %s: %v", fork.Name, err)
								}
							})

							if fork.Sudo {
								t.Fatal("fork reports sudo enabled, want disabled")
							}
							verifyCmd := "if id -nG | tr ' ' '\\n' | grep -qx sudo; then echo user-still-in-sudo-group; exit 1; fi" +
								"; if sudo -n true >/tmp/md-sudo-check 2>&1; then echo sudo-still-works; exit 1; fi" +
								"; cat /tmp/md-sudo-check"
							out, err = fork.runCmd(t.Context(), "", fork.SSHCommand(nil, verifyCmd))
							if err != nil {
								t.Fatalf("verify sudo removed: %v\n%s", err, out)
							}
						})
					})

					t.Run("file_io", func(t *testing.T) {
						if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "echo hello > /tmp/smoke-test && cat /tmp/smoke-test")); err != nil {
							t.Fatalf("file I/O: %v", err)
						}
					})

					t.Run("timezone", func(t *testing.T) {
						// The host /etc/localtime mount must not follow a symlink and
						// clobber /usr/share/zoneinfo/Etc/UTC.
						out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "TZ=UTC date +%z"))
						if err != nil {
							t.Fatalf("TZ=UTC date: %v", err)
						}
						if out != "+0000" {
							t.Fatalf("TZ=UTC date +%%z = %q, want +0000", out)
						}
					})

					t.Run("list", func(t *testing.T) {
						containers, err := client.List(t.Context())
						if err != nil {
							t.Fatalf("List: %v", err)
						}
						var found *Container
						for _, c := range containers {
							if c.Name == ct.Name {
								found = c
								break
							}
						}
						if found == nil {
							t.Fatalf("container %s not found in list output", ct.Name)
						}
						if found.SSHPort <= 0 {
							t.Errorf("SSHPort is %d, expected positive", found.SSHPort)
						} else {
							t.Logf("SSHPort=%d", found.SSHPort)
						}
						if found.VNCPort > 0 {
							t.Logf("VNCPort=%d", found.VNCPort)
						}
						if found.Sudo != ct.Sudo {
							t.Errorf("Sudo = %v, want %v", found.Sudo, ct.Sudo)
						}
						t.Logf("Sudo=%v", found.Sudo)
					})
				})

				t.Run("mounts", func(t *testing.T) {
					// Bind-mount two host directories: one writable, one
					// read-only. This guards the runtime -v mount contract,
					// which is distinct from cache injection (caches are
					// COPYed into the image at build time, not mounted). The
					// unprivileged "user" account must read and write a
					// writable mount while the host keeps ownership of the
					// files, and a read-only mount must be readable but reject
					// writes. Under rootless podman this depends on
					// --userns=keep-id mapping the host user to "user".
					//
					// The mount directories are left host-user-owned at 0700,
					// mirroring how AgentMounts exposes the host's real config
					// dirs (e.g. ~/.claude), so the test exercises the actual
					// invariant rather than a permissive world-writable dir.
					hostRW := t.TempDir()
					hostRO := t.TempDir()
					if err := os.WriteFile(filepath.Join(hostRW, "seed.txt"), []byte("rw-seed\n"), 0o600); err != nil {
						t.Fatalf("seed writable mount: %v", err)
					}
					if err := os.WriteFile(filepath.Join(hostRO, "seed.txt"), []byte("ro-seed\n"), 0o600); err != nil {
						t.Fatalf("seed read-only mount: %v", err)
					}

					ct, err := client.Container()
					if err != nil {
						t.Fatalf("Container: %v", err)
					}
					ct.Name = "md-smoke-" + rt + "-mounts"
					removeSmokeContainerIfPresent(t, t.Context(), client, ct.Name)
					opts := &StartOpts{
						BaseImage: baseImage,
						Quiet:     true,
						Caches:    sharedCaches,
						Mounts: []Mount{
							{HostPath: hostRW, ContainerPath: "/home/user/mnt-rw"},
							{HostPath: hostRO, ContainerPath: "/home/user/mnt-ro", ReadOnly: true},
						},
					}
					if err := ct.Launch(t.Context(), os.Stdout, os.Stderr, opts); err != nil {
						t.Fatalf("Launch: %v", err)
					}
					t.Cleanup(func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
						defer cancel()
						if err := ct.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
							t.Logf("cleanup %s: %v", ct.Name, err)
						}
					})
					if _, err := ct.Connect(t.Context(), io.Discard, io.Discard, opts); err != nil {
						t.Fatalf("Connect: %v", err)
					}

					t.Run("writable", func(t *testing.T) {
						// Read the host-seeded file and write a new one, all as
						// the unprivileged "user" account.
						writeCmd := `test "$(id -un)" = user || { echo "unexpected user: $(id -un)"; exit 1; }`
						if rootlessRuntime {
							writeCmd += ` && test "$(id -u)" = 1000 || { echo "unexpected uid: $(id -u)"; exit 1; }`
						}
						writeCmd += ` && cat /home/user/mnt-rw/seed.txt` +
							` && printf 'from-container\n' > /home/user/mnt-rw/written.txt`
						out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, writeCmd))
						if err != nil {
							t.Fatalf("read+write writable mount: %v\n%s", err, out)
						}
						if !strings.Contains(out, "rw-seed") {
							t.Fatalf("writable mount read = %q, want to contain %q", out, "rw-seed")
						}
						// The container's write must be visible back on the
						// host with the expected content.
						data, err := os.ReadFile(filepath.Join(hostRW, "written.txt")) //nolint:gosec // hostRW is a test temp dir.
						if err != nil {
							t.Fatalf("read container-written file on host: %v", err)
						}
						if strings.TrimSpace(string(data)) != "from-container" {
							t.Fatalf("host content = %q, want %q", strings.TrimSpace(string(data)), "from-container")
						}
						// The host user must retain ownership: it can still
						// overwrite the file the container created. A mount mode
						// that chowned the tree to a subuid (e.g. dropping
						// keep-id in favor of ":U") would make this fail with
						// EACCES.
						if err := os.WriteFile(filepath.Join(hostRW, "written.txt"), []byte("from-host\n"), 0o600); err != nil {
							t.Fatalf("host cannot overwrite container-created file (mount lost host ownership): %v", err)
						}
					})

					t.Run("readonly", func(t *testing.T) {
						out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "cat /home/user/mnt-ro/seed.txt"))
						if err != nil {
							t.Fatalf("read read-only mount: %v\n%s", err, out)
						}
						if !strings.Contains(out, "ro-seed") {
							t.Fatalf("read-only mount read = %q, want to contain %q", out, "ro-seed")
						}
						// A write must be rejected by the read-only mount and
						// must not leak to the host.
						if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "printf x > /home/user/mnt-ro/blocked.txt")); err == nil {
							t.Fatal("write to read-only mount succeeded, want failure")
						}
						if _, err := os.Stat(filepath.Join(hostRO, "blocked.txt")); !os.IsNotExist(err) {
							t.Fatalf("read-only mount leaked a write to the host: err=%v", err)
						}
					})
				})

				t.Run("nested", func(t *testing.T) {
					if !nestedOK {
						t.Skip("skipping: nested newuidmap fails with rootless podman (user namespace stacking)")
					}
					ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-nested", true, sharedCaches...)

					t.Run("version", func(t *testing.T) {
						out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "podman version --format '{{.Version}}'"))
						if err != nil {
							t.Fatalf("podman version: %v", err)
						}
						if out == "" {
							t.Fatal("podman returned empty version")
						} else {
							t.Logf("nested podman version: %s", out)
						}
					})

					t.Run("info", func(t *testing.T) {
						out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "podman info --format '{{.Host.RemoteSocket.Path}}'"))
						if err != nil {
							t.Fatalf("podman info: %v", err)
						}
						t.Logf("nested podman socket: %s", out)
					})

					t.Run("run_alpine", func(t *testing.T) {
						subCtx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
						defer cancel()
						out, err := ct.runCmd(subCtx, "", ct.SSHCommand(nil, "podman run --rm docker.io/alpine:latest echo hello-from-nested-podman"))
						if err != nil {
							t.Fatalf("podman run alpine: %v", err)
						}
						if out != "hello-from-nested-podman" {
							t.Fatalf("expected 'hello-from-nested-podman', got %q", out)
						}
						t.Logf("nested podman run: %s", out)
					})

					t.Run("run_alpine_id", func(t *testing.T) {
						subCtx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
						defer cancel()
						out, err := ct.runCmd(subCtx, "", ct.SSHCommand(nil, "podman run --rm docker.io/alpine:latest id -u"))
						if err != nil {
							t.Fatalf("podman run id: %v", err)
						}
						got := strings.TrimSpace(out)
						if got != "0" {
							t.Logf("nested container UID: %s (may be 0 via user namespace)", got)
						}
					})

					t.Run("pull_busybox", func(t *testing.T) {
						subCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
						defer cancel()
						out, err := ct.runCmd(subCtx, "", ct.SSHCommand(nil, "podman pull docker.io/busybox:latest"))
						if err != nil {
							t.Fatalf("podman pull busybox: %v", err)
						}
						t.Logf("pull output: %s", out)
					})

					t.Run("run_busybox", func(t *testing.T) {
						subCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
						defer cancel()
						out, err := ct.runCmd(subCtx, "", ct.SSHCommand(nil, "podman run --rm docker.io/busybox:latest echo ok"))
						if err != nil {
							t.Fatalf("podman run busybox: %v", err)
						}
						if out != "ok" {
							t.Fatalf("expected 'ok', got %q", out)
						}
					})
				})

				t.Run("lifecycle", func(t *testing.T) {
					ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-lifecycle", false, sharedCaches...)

					// Verify Status returns "running" after launch.
					if s := ct.Status(t.Context()); s != "running" {
						t.Fatalf("Status after launch: expected 'running', got %q", s)
					}

					if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "echo persisted > /tmp/smoke-test")); err != nil {
						t.Fatalf("write file: %v", err)
					}

					t.Log("stopping container ...")
					if err := ct.Stop(t.Context()); err != nil {
						t.Fatalf("Stop: %v", err)
					}

					// Verify Status returns "exited" after stop.
					if s := ct.Status(t.Context()); s != "exited" {
						t.Fatalf("Status after stop: expected 'exited', got %q", s)
					}

					t.Log("reviving container (like md start on stopped container) ...")
					if err := ct.Revive(t.Context(), io.Discard, io.Discard); err != nil {
						t.Fatalf("Revive: %v", err)
					}

					// Verify Status returns "running" after revive.
					if s := ct.Status(t.Context()); s != "running" {
						t.Fatalf("Status after revive: expected 'running', got %q", s)
					}

					// Verify SSH works and container state is preserved.
					out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "cat /tmp/smoke-test"))
					if err != nil {
						t.Fatalf("read file after revive: %v", err)
					}
					if got := strings.TrimSpace(out); got != "persisted" {
						t.Fatalf("expected 'persisted', got %q", got)
					}
				})
			})

			t.Run("repo_workflow", func(t *testing.T) {
				repo := createSmokeGitRepo(t, "main", "main", false)
				cp := "/home/user/src/smoke-" + rt + "-repo"
				ct := launchSmokeRepoContainer(t, t.Context(), client, baseImage, sharedCaches, &Repo{
					GitRoot:       repo,
					Branches:      []string{"main"},
					ContainerPath: cp,
				})

				t.Run("origin_refs", func(t *testing.T) {
					mainCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/origin/main")
					assertSmokeContainerGitRef(t, ct, cp, "refs/remotes/origin/main", mainCommit)
					assertSmokeContainerGitRefMissing(t, ct, cp, containerIncomingRefPrefix+"main")
					assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/host/main")
					assertSmokeContainerGitRefMissing(t, ct, cp, "base")
					assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/origin/HEAD")
					assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/host/HEAD")
				})

				t.Run("push_pull", func(t *testing.T) {
					if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("local-push\n"), 0o600); err != nil {
						t.Fatalf("write local README.md: %v", err)
					}
					runSmokeGit(t, t.Context(), repo, "commit", "-q", "-am", "local push")

					backupBranch, err := ct.Push(t.Context(), io.Discard, io.Discard, 0)
					if err != nil {
						t.Fatalf("Push: %v", err)
					}
					if !strings.HasPrefix(backupBranch, "backup-") {
						t.Fatalf("backup branch = %q, want backup-*", backupBranch)
					}
					out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "cat "+shellQuote(cp+"/README.md")))
					if err != nil {
						t.Fatalf("read pushed README.md: %v", err)
					}
					if got := strings.TrimSpace(out); got != "local-push" {
						t.Fatalf("container README.md = %q, want local-push", got)
					}
					assertSmokeContainerNoDiff(t, ct, 0)

					if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "printf 'container-pull\n' > "+shellQuote(cp+"/README.md"))); err != nil {
						t.Fatalf("write container README.md: %v", err)
					}
					if err := ct.Pull(t.Context(), io.Discard, io.Discard, 0, nil); err != nil {
						t.Fatalf("Pull: %v", err)
					}
					data, err := os.ReadFile(filepath.Join(repo, "README.md")) //nolint:gosec // repo is a test temp dir.
					if err != nil {
						t.Fatalf("read local README.md: %v", err)
					}
					if got := strings.TrimSpace(string(data)); got != "container-pull" {
						t.Fatalf("local README.md = %q, want container-pull", got)
					}
					if got := runSmokeGit(t, t.Context(), repo, "status", "--short"); got != "" {
						t.Fatalf("local repo is dirty after Pull:\n%s", got)
					}
					assertSmokeContainerNoDiff(t, ct, 0)
				})

				t.Run("fork", func(t *testing.T) {
					staleForkName := containerName(sanitizeDockerName(filepath.Base(cp)), "main-0")
					removeSmokeContainerIfPresent(t, t.Context(), client, staleForkName)
					removeSmokeImageIfPresent(t, t.Context(), client, "md-fork-"+ct.Name)
					t.Cleanup(func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
						defer cancel()
						removeSmokeImageIfPresent(t, cleanupCtx, client, "md-fork-"+ct.Name)
					})

					prepareForkCmd := "printf snapshot > /tmp/fork-marker" +
						" && cd " + shellQuote(cp) +
						" && printf 'fork-committed\n' > fork-committed.txt" +
						" && git add fork-committed.txt" +
						" && git commit -q -m 'fork committed'" +
						" && printf 'fork-uncommitted\n' > README.md"
					if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, prepareForkCmd)); err != nil {
						t.Fatalf("prepare source for Fork: %v", err)
					}

					if err := ct.Stop(t.Context()); err != nil {
						t.Fatalf("stop source before Fork: %v", err)
					}
					if got := ct.Status(t.Context()); got != "exited" {
						t.Fatalf("source state after Stop = %q, want exited", got)
					}
					sourceStartedAt, err := client.Runtime.Run(t.Context(), "", "inspect", "--format", "{{.State.StartedAt}}", ct.Name)
					if err != nil {
						t.Fatalf("inspect source start time: %v", err)
					}

					var forkStdout, forkStderr strings.Builder
					mounts, err := ct.AgentMounts(slices.Collect(maps.Values(HarnessMounts))...)
					if err != nil {
						t.Fatalf("AgentMounts: %v", err)
					}
					// Add a brand-new repo (not in the source container) at an explicit
					// container path to exercise ForkRepo.ContainerPath.
					extraRepo := createSmokeGitRepo(t, "main", "main", false)
					extraMountedPath := "/home/user/src/smoke-" + rt + "-fork-extra"
					fork, err := ct.Fork(t.Context(), &forkStdout, &forkStderr, &ForkOpts{
						Repos: []ForkRepo{
							{GitRoot: repo, SourceBranches: ct.Repos[0].Branches, DestPrimary: "main-0"},
							{GitRoot: extraRepo, SourceBranches: []string{"main"}, ContainerPath: extraMountedPath, DestPrimary: "extra-0"},
						},
						Quiet:   true,
						Mounts:  mounts,
						MaxCPUs: DefaultMaxCPUs(),
					})
					if err != nil {
						state, stateErr := client.Runtime.Run(t.Context(), "", "inspect", "--format", "{{json .State}}", staleForkName)
						if stateErr != nil {
							state = fmt.Sprintf("inspect state failed: %v", stateErr)
						}
						logs, logsErr := client.Runtime.Run(t.Context(), "", "logs", staleForkName)
						if logsErr != nil {
							logs = fmt.Sprintf("logs failed: %v", logsErr)
						}
						sshDiag, sshDiagErr := client.Runtime.Run(t.Context(), "", "exec", staleForkName, "bash", "-lc", strings.Join([]string{
							"stat -c '%U:%G %a %n' /home/user /home/user/.ssh /home/user/.ssh/authorized_keys /etc/ssh/ssh_host_ed25519_key 2>&1",
							"pgrep -a sshd 2>&1 || true",
							"tail -120 /var/log/auth.log 2>&1 || true",
							"/usr/sbin/sshd -T 2>&1 | head -80 || true",
						}, "; "))
						if sshDiagErr != nil {
							sshDiag = fmt.Sprintf("ssh diagnostics failed: %v", sshDiagErr)
						}
						t.Fatalf("Fork: %v\nstdout:\n%s\nstderr:\n%s\nstate:\n%s\nlogs:\n%s\nssh diagnostics:\n%s", err, forkStdout.String(), forkStderr.String(), state, logs, sshDiag)
					}
					t.Cleanup(func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
						defer cancel()
						if err := fork.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
							t.Logf("cleanup %s: %v", fork.Name, err)
						}
					})

					if _, err := client.Runtime.Run(t.Context(), "", "image", "inspect", "md-fork-"+ct.Name); err == nil {
						t.Fatalf("temporary fork snapshot tag md-fork-%s still exists", ct.Name)
					}

					if got := ct.Status(t.Context()); got != "exited" {
						t.Fatalf("source state after Fork = %q, want exited", got)
					}
					if got, err := client.Runtime.Run(t.Context(), "", "inspect", "--format", "{{.State.StartedAt}}", ct.Name); err != nil {
						t.Fatalf("inspect source start time after Fork: %v", err)
					} else if got != sourceStartedAt {
						t.Fatalf("source start time changed from %q to %q", sourceStartedAt, got)
					}

					if len(fork.Repos) != 2 {
						t.Fatalf("fork repos = %d, want 2", len(fork.Repos))
					}
					if fork.Repos[0].Branches[0] != "main-0" {
						t.Fatalf("fork branch = %q, want main-0", fork.Repos[0].Branches[0])
					}
					if fork.Repos[1].ContainerPath != extraMountedPath {
						t.Fatalf("fork extra repo path = %q, want %q", fork.Repos[1].ContainerPath, extraMountedPath)
					}
					if got := runSmokeGit(t, t.Context(), repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "main-0@{upstream}"); got != "origin/main" {
						t.Fatalf("host fork upstream = %q, want origin/main", got)
					}
					out, err := fork.runCmd(t.Context(), "", fork.SSHCommand(nil, "cat /tmp/fork-marker && printf '\n' && git -C "+shellQuote(cp)+" branch --show-current && git -C "+shellQuote(cp)+" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' && cat "+shellQuote(cp+"/fork-committed.txt")+" && cat "+shellQuote(cp+"/README.md")+" && git -C "+shellQuote(cp)+" diff --name-only "+shellQuote(containerSyncRefPrefix+"main-0")))
					if err != nil {
						t.Fatalf("inspect fork: %v", err)
					}
					for _, want := range []string{"snapshot", "main-0", "origin/main", "fork-committed", "fork-uncommitted", "README.md"} {
						if !strings.Contains(out, want) {
							t.Fatalf("fork output missing %q:\n%s", want, out)
						}
					}
					// The extra repo was initialized at its explicit ContainerPath, on its
					// own branch, with the pushed content.
					extraOut, err := fork.runCmd(t.Context(), "", fork.SSHCommand(nil, "git -C "+shellQuote(extraMountedPath)+" branch --show-current && cat "+shellQuote(extraMountedPath+"/README.md")))
					if err != nil {
						t.Fatalf("inspect fork extra repo at %s: %v", extraMountedPath, err)
					}
					for _, want := range []string{"extra-0", "initial"} {
						if !strings.Contains(extraOut, want) {
							t.Fatalf("fork extra repo output missing %q:\n%s", want, extraOut)
						}
					}
				})
			})

			t.Run("run_apply_patch", func(t *testing.T) {
				repo := createSmokeGitRepo(t, "main", "main", false)
				args := []string{
					"run",
					"-image", baseImage,
					"-repo", repo,
					"-branch", "main",
					"-no-caches",
				}
				if rootlessRuntime {
					args = append(args, "-cache", smokeCacheSource+":/home/user/.cache/smoke")
				}
				args = append(args,
					"-apply-patch",
					"bash", "-c", "echo foo > bar.txt",
				)
				runSmokeMD(t, client, args...)
				data, err := os.ReadFile(filepath.Join(repo, "bar.txt")) //nolint:gosec // repo is a test temp dir.
				if err != nil {
					t.Fatalf("read applied file: %v", err)
				}
				if got := strings.TrimSpace(string(data)); got != "foo" {
					t.Fatalf("bar.txt = %q, want foo", got)
				}
				if got := runSmokeGit(t, t.Context(), repo, "status", "--short"); got != "" {
					t.Fatalf("local repo is dirty after md run -apply-patch:\n%s", got)
				}
			})

			t.Run("diff_rebase_in_progress", func(t *testing.T) {
				repo := createSmokeGitRepo(t, "main", "main", false)
				cp := "/home/user/src/smoke-" + rt + "-rebase"
				ct := launchSmokeRepoContainer(t, t.Context(), client, baseImage, sharedCaches, &Repo{
					GitRoot:       repo,
					Branches:      []string{"main"},
					ContainerPath: cp,
				})

				prepareRebaseCmd := strings.Join([]string{
					"cd " + shellQuote(cp),
					"git config user.name 'Smoke Test'",
					"git config user.email smoke@example.invalid",
					"git checkout -q -b rebase-target",
					"printf 'target\n' > README.md",
					"git commit -q -am target",
					"git checkout -q main",
					"printf 'container\n' > README.md",
					"git commit -q -am container",
					"if git rebase rebase-target >/tmp/md-rebase.out 2>/tmp/md-rebase.err; then echo 'rebase unexpectedly succeeded' >&2; exit 1; fi",
					"test -s .git/rebase-merge/head-name",
				}, " && ")
				if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, prepareRebaseCmd)); err != nil {
					t.Fatalf("prepare in-progress rebase: %v", err)
				}

				out := runSmokeMD(t, client,
					"diff",
					"-repo", repo,
					"-branch", "main",
					"--name-only",
				)
				if got := strings.TrimSpace(out); got != "README.md" {
					t.Fatalf("md diff --name-only = %q, want README.md", got)
				}
			})

			t.Run("repo_remote_refs_non_default_base_branch", func(t *testing.T) {
				repo := createSmokeGitRepoWithRemote(t, "upstream", "release", "feature", true)
				cp := "/home/user/src/smoke-" + rt + "-non-default-base"
				ct := launchSmokeRepoContainer(t, t.Context(), client, baseImage, sharedCaches, &Repo{
					GitRoot:       repo,
					Branches:      []string{"feature"},
					ContainerPath: cp,
				})

				releaseCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/upstream/release")
				upstreamFeatureCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/upstream/feature")
				localFeatureCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "feature")
				assertSmokeContainerGitRef(t, ct, cp, "refs/remotes/upstream/release", releaseCommit)
				assertSmokeContainerGitRef(t, ct, cp, "refs/remotes/upstream/feature", upstreamFeatureCommit)
				assertSmokeContainerGitRefMissing(t, ct, cp, containerIncomingRefPrefix+"release")
				assertSmokeContainerGitRefMissing(t, ct, cp, containerIncomingRefPrefix+"feature")
				assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/host/release")
				assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/host/feature")
				assertSmokeContainerGitRef(t, ct, cp, "feature", localFeatureCommit)
				assertSmokeContainerGitRefMissing(t, ct, cp, "base")
				if upstreamFeatureCommit == localFeatureCommit {
					t.Fatal("test setup error: upstream feature equals local feature; expected an unpushed commit")
				}
				assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/upstream/HEAD")
				assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/origin/release")
				assertSmokeContainerGitRefMissing(t, ct, cp, "refs/remotes/host/HEAD")
			})

			// Cache subtest. Rootless Podman reuses the shared cache-bearing image
			// to avoid preparing an ID mapping for a second copy of the large base.
			// Do not run this in parallel with build_image below: build_image removes
			// and rebuilds md-user-local, which this test uses as the Dockerfile FROM
			// image. Podman then tries to resolve the missing local short name via
			// registries.conf and fails when no unqualified registry is configured.
			t.Run("cache", func(t *testing.T) {
				cacheMounts := sharedCaches
				if len(cacheMounts) == 0 {
					src := t.TempDir()
					if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("cache-works"), 0o600); err != nil {
						t.Fatal(err)
					}
					cacheMounts = []CacheMount{{
						Name:          "smoke-cache",
						HostPath:      src,
						ContainerPath: "/home/user/.cache/smoke",
					}}
				}
				ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-cache", false, cacheMounts...)

				out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "cat /home/user/.cache/smoke/hello.txt"))
				if err != nil {
					t.Fatalf("read cached file: %v", err)
				}
				if got := strings.TrimSpace(out); got != "cache-works" {
					t.Fatalf("expected 'cache-works', got %q", got)
				}
				t.Log("cache injection works")
			})

			// Clean rebuild test: independent, runs in parallel.
			t.Run("build_image", func(t *testing.T) {
				t.Parallel()
				if testing.Short() {
					t.Skip("skipping: clean rebuild in short mode")
				}
				subCtx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
				defer cancel()

				for _, img := range []string{"md-root-local", "md-user-local"} {
					if hasImage(subCtx, client, img) {
						t.Logf("removing existing image %s for clean build test", img)
						removeSmokeImageIfPresent(t, subCtx, client, img)
					}
				}

				t.Log("building images from scratch ...")
				if err := client.BuildImage(subCtx, os.Stdout, os.Stderr, PlatformDefault); err != nil {
					t.Fatalf("BuildImage: %v", err)
				}

				for _, img := range []string{"md-root-local", "md-user-local"} {
					if !hasImage(subCtx, client, img) {
						t.Errorf("image %s not found after build", img)
					}
				}

				labels := []string{
					"org.opencontainers.image.source",
					"org.opencontainers.image.licenses",
				}
				for _, label := range labels {
					out, err := client.Runtime.Run(subCtx, "", "image", "inspect", "--format",
						fmt.Sprintf("{{index .Config.Labels %q}}", label), "md-user-local")
					if err != nil {
						t.Errorf("inspecting label %s: %v", label, err)
					} else if out == "" || out == "<no value>" {
						t.Errorf("label %s missing from md-user-local", label)
					}
				}
			})

			// Foreign base images: md must run on an image it does not control.
			// debian:stable-slim is the reference Debian-family base for both halves
			// of the md startup capability contract: a reduced image that satisfies
			// it reaches SSH, and an image that does not refuses to start, naming the
			// missing capability and the md option that requested it.
			t.Run("foreign_base", func(t *testing.T) {
				if os.Getuid() == 0 {
					// md running as root chowns the specialized image to user:user,
					// which a base image without the account cannot resolve.
					t.Skip("skipping: md as root needs the user account in the base image")
				}
				slim := "debian:stable-slim"
				reduced := ensureSmokeForeignBase(t, t.Context(), client)

				t.Run("reduced_image_reaches_ssh", func(t *testing.T) {
					prebuildSpecializedImage(t, t.Context(), client, reduced, nil)
					ct := launchSmokeContainer(t, t.Context(), client, reduced, rt+"-debian-slim", false)

					// start.sh provisions the account, since the base image has none.
					logs, err := client.Runtime.Run(t.Context(), "", "logs", ct.Name)
					if err != nil {
						t.Fatalf("logs: %v", err)
					}
					for _, want := range []string{
						"created the user account (UID/GID 1000)",
						// The reduced image has neither Xvnc nor DBus: unrequested
						// subsystems must be skipped, not fail startup.
						"MD_DISPLAY not set, skipping X/VNC startup",
						"Skipping DBus",
					} {
						if !strings.Contains(logs, want) {
							t.Errorf("container log does not contain %q:\n%s", want, logs)
						}
					}

					// Docker passes the host UID/GID through, rootless Podman maps the
					// host user onto the fixed UID/GID 1000 contract in docs/ROOTLESS.md.
					wantUID, wantGID := os.Getuid(), os.Getgid()
					if client.Runtime.IsRootless() {
						wantUID, wantGID = containerUserUID, containerUserGID
					}
					wantOwner := fmt.Sprintf("%d:%d", wantUID, wantGID)
					out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil,
						"id -un; id -u; id -g; stat -c %u:%g /home/user /home/user/.ssh/authorized_keys; git --version"))
					if err != nil {
						t.Fatalf("inspecting reduced container: %v", err)
					}
					lines := strings.Split(strings.TrimSpace(out), "\n")
					if len(lines) != 6 {
						t.Fatalf("expected 6 lines of container state, got %d:\n%s", len(lines), out)
					}
					for i, want := range []string{"user", strconv.Itoa(wantUID), strconv.Itoa(wantGID), wantOwner, wantOwner} {
						if got := strings.TrimSpace(lines[i]); got != want {
							t.Errorf("container state line %d = %q, want %q", i+1, got, want)
						}
					}
					if got := strings.TrimSpace(lines[5]); !strings.HasPrefix(got, "git version") {
						t.Errorf("git --version = %q, want a git version (md's post-SSH provisioning needs git)", got)
					}
				})

				t.Run("repo_workflow", func(t *testing.T) {
					// md pushes a repository over SSH into a tree it initializes as
					// `user`, so this covers a home start.sh had to create for an account
					// the base image never had.
					repo := createSmokeGitRepo(t, "main", "main", false)
					cp := "/home/user/src/smoke-" + rt + "-debian-slim-repo"
					ct := launchSmokeRepoContainer(t, t.Context(), client, reduced, nil, &Repo{
						GitRoot:       repo,
						Branches:      []string{"main"},
						ContainerPath: cp,
					})

					// The refs md pushed and the resulting working tree survived the
					// transfer.
					mainCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/origin/main")
					assertSmokeContainerGitRef(t, ct, cp, "refs/remotes/origin/main", mainCommit)
					assertSmokeContainerNoDiff(t, ct, 0)

					// Everything md created for the account must belong to `user`: the
					// base image has no `user`, so the specialized image cannot pre-own
					// the home it copied the SSH key into.
					out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil,
						"stat -c %U:%G "+cp+" && find "+cp+" -not -user user -print"))
					if err != nil {
						t.Fatalf("checking ownership of %s: %v\n%s", cp, err, out)
					}
					lines := strings.Split(strings.TrimSpace(out), "\n")
					if len(lines) == 0 || lines[0] != "user:user" {
						t.Errorf("%s owner = %q, want user:user", cp, out)
					} else if len(lines) > 1 {
						t.Errorf("paths not owned by user:\n%s", strings.Join(lines[1:], "\n"))
					}

					// A clone and a commit in the container prove `user` can read and
					// write the repository, not merely have refs pushed at it. The
					// identity is explicit because the host identity may be unset.
					runSmokeContainerGit(t, ct, cp, "-c", "user.name=smoke", "-c", "user.email=smoke@example.invalid",
						"commit", "-q", "--allow-empty", "-m", "smoke commit")
					head := runSmokeContainerGit(t, ct, cp, "rev-parse", "HEAD")
					clone := "/home/user/src/smoke-" + rt + "-debian-slim-clone"
					runSmokeContainerGit(t, ct, cp, "clone", "--quiet", ".", clone)
					if got := runSmokeContainerGit(t, ct, clone, "rev-parse", "HEAD"); got != head {
						t.Errorf("clone HEAD = %q, want %q", got, head)
					}
				})

				t.Run("missing_requested_capability", func(t *testing.T) {
					// -display on an image without Xvnc must refuse to start.
					ct := newSmokeContainer(t, t.Context(), client, "md-smoke-debian-slim-display")
					opts := &StartOpts{BaseImage: reduced, Display: true, Quiet: true}
					if err := ct.Launch(t.Context(), io.Discard, io.Discard, opts); err != nil {
						t.Fatalf("Launch: %v", err)
					}
					code, logs := waitForSmokeContainerExit(t, t.Context(), client, ct.Name)
					if code != 1 {
						t.Errorf("exit code = %d, want 1:\n%s", code, logs)
					}
					for _, want := range []string{"the desktop capability is missing", "startxfce4", "-display"} {
						if !strings.Contains(logs, want) {
							t.Errorf("container log does not contain %q:\n%s", want, logs)
						}
					}
				})

				t.Run("missing_mandatory_capability", func(t *testing.T) {
					// Plain debian:stable-slim has no sshd at all: md cannot manage it.
					prebuildSpecializedImage(t, t.Context(), client, slim, nil)
					ct := newSmokeContainer(t, t.Context(), client, "md-smoke-debian-slim-nosshd")
					opts := &StartOpts{BaseImage: slim, Quiet: true}
					if err := ct.Launch(t.Context(), io.Discard, io.Discard, opts); err != nil {
						t.Fatalf("Launch: %v", err)
					}
					code, logs := waitForSmokeContainerExit(t, t.Context(), client, ct.Name)
					if code != 1 {
						t.Errorf("exit code = %d, want 1:\n%s", code, logs)
					}
					for _, want := range []string{"md startup is missing sshd", "requested by md start"} {
						if !strings.Contains(logs, want) {
							t.Errorf("container log does not contain %q:\n%s", want, logs)
						}
					}
				})
			})
		})
	}
}
