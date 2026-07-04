// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// End-to-end container lifecycle smoke tests.

//go:build smoke

package md

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

	client, err := newClient(tmpHome, rt, io.Discard)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	client.Logger = testLogger(t)
	client.env = []string{
		"HOME=" + tmpHome,
		"GIT_SSH_COMMAND=ssh -F " + filepath.Join(tmpHome, ".ssh", "config"),
		"XDG_CONFIG_HOME=" + filepath.Join(tmpHome, ".config"),
	}

	// podman system reset cleans up overlay storage before t.TempDir removal,
	// avoiding permission errors.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		_, _ = client.runCmd(ctx, "", []string{rt, "system", "reset", "-f"})
	})
	return client
}

// hasImage checks whether a container image exists in the local store.
func hasImage(ctx context.Context, c *Client, name string) bool {
	_, err := c.runCmd(ctx, "", []string{c.Runtime, "image", "inspect", "--format", "{{.Id}}", name})
	return err == nil
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

// prebuildSpecializedImage builds the specialized image (with SSH keys and no
// caches) so that subsequent subtests can reuse it without racing on the build.
func prebuildSpecializedImage(t *testing.T, ctx context.Context, c *Client, baseImage string) {
	ct, err := c.Container()
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	ct.Name = "md-smoke-prebuild"
	opts := &StartOpts{BaseImage: baseImage, Quiet: true}
	if _, err := ct.ensureImage(ctx, io.Discard, io.Discard, baseImage, opts.Platform, opts.Caches, true); err != nil {
		t.Fatalf("prebuild specialized image: %v", err)
	}
}

// launchSmokeContainer creates a Container with the given name suffix and
// calls Launch+Connect. Returns the live container (caller must Purge via
// t.Cleanup).
func launchSmokeContainer(t *testing.T, ctx context.Context, c *Client, baseImage, nameSuffix string, sudo bool, caches ...CacheMount) *Container {
	if sudo && isRootlessPodman(c.Runtime) {
		t.Skip("skipping: sudo is not supported with rootless podman")
	}
	ct, err := c.Container()
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	ct.Name = "md-smoke-" + nameSuffix

	_, _ = c.runCmd(ctx, "", []string{c.Runtime, "rm", "-f", "-v", ct.Name})

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

func launchSmokeRepoContainer(t *testing.T, ctx context.Context, c *Client, baseImage string, repo *Repo) *Container {
	ct, err := c.Container(*repo)
	if err != nil {
		t.Fatalf("Container: %v", err)
	}

	_, _ = c.runCmd(ctx, "", []string{c.Runtime, "rm", "-f", "-v", ct.Name})

	opts := &StartOpts{
		BaseImage: baseImage,
		Quiet:     true,
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
	goArgs := append([]string{"run", "./cmd/md", "--runtime", c.Runtime}, args...)
	cmd := exec.CommandContext(ctx, "go", goArgs...) //nolint:gosec // args are test-controlled.
	cmd.Dir = repoRoot
	overrides := append([]string(nil), c.env...)
	overrides = append(overrides, "LANG=C")
	overrides = append(overrides, smokeGoEnv(t)...)
	cmd.Env = envWithOverrides(os.Environ(), overrides)
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

func assertSmokeContainerNoDiff(t *testing.T, ct *Container, repoPath string) {
	gitArgs := []string{"git", "-C", repoPath, "diff", "--exit-code", "@{upstream}", "--", "."}
	out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, shellQuoteArgs(gitArgs)))
	if err != nil {
		t.Fatalf("container diff against upstream is not empty: %v\n%s", err, out)
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

			// Rootless podman adds --userns=keep-id, which puts the
			// inner container in a user namespace. Nested newuidmap
			// then fails with EPERM — user namespace stacking is not
			// supported. Rootful docker and rootful podman (uid 0)
			// don't have this limitation.
			// Error: "newuidmap: write to uid_map failed: Operation not permitted"
			nestedOK := rt != "podman" || os.Getuid() == 0

			// Fetch md-user upfront so all subtests can reuse it.
			baseImage := ensureImages(t, t.Context(), client)

			// Pre-build the specialized image once so the serialized
			// subtests (launch, nested, lifecycle) don't race on the
			// build. The cache subtest uses different caches so it
			// produces a different image and runs in parallel.
			prebuildSpecializedImage(t, t.Context(), client, baseImage)

			// Serialized group: these subtests share the same
			// specialized image, so running them sequentially avoids
			// redundant image-build checks.
			t.Run("serialized", func(t *testing.T) {
				t.Run("launch", func(t *testing.T) {
					ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-launch", false)

					t.Run("sudo", func(t *testing.T) {
						ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-sudo", true)
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
								_, _ = client.runCmd(cleanupCtx, "", []string{client.Runtime, "rmi", "-f", cleanupImage})
							})

							var forkStdout, forkStderr strings.Builder
							fork, err := ct.Fork(t.Context(), &forkStdout, &forkStderr, &ForkOpts{
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

				t.Run("nested", func(t *testing.T) {
					if !nestedOK {
						t.Skip("skipping: nested newuidmap fails with rootless podman (user namespace stacking)")
					}
					ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-nested", true)

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
					ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-lifecycle", false)

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
				mountedPath := "/home/user/src/smoke-" + rt + "-repo"
				ct := launchSmokeRepoContainer(t, t.Context(), client, baseImage, &Repo{
					GitRoot:     repo,
					Branch:      "main",
					MountedPath: mountedPath,
				})

				t.Run("origin_refs", func(t *testing.T) {
					mainCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/origin/main")
					assertSmokeContainerGitRef(t, ct, mountedPath, "refs/remotes/origin/main", mainCommit)
					assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/host/main")
					assertSmokeContainerGitRefMissing(t, ct, mountedPath, "base")
					assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/origin/HEAD")
					assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/host/HEAD")
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
					out, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "cat "+shellQuote(mountedPath+"/README.md")))
					if err != nil {
						t.Fatalf("read pushed README.md: %v", err)
					}
					if got := strings.TrimSpace(out); got != "local-push" {
						t.Fatalf("container README.md = %q, want local-push", got)
					}
					assertSmokeContainerNoDiff(t, ct, mountedPath)

					if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, "printf 'container-pull\n' > "+shellQuote(mountedPath+"/README.md"))); err != nil {
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
					assertSmokeContainerNoDiff(t, ct, mountedPath)
				})

				t.Run("fork", func(t *testing.T) {
					staleForkName := containerName(sanitizeDockerName(filepath.Base(mountedPath)), "main-0")
					_, _ = client.runCmd(t.Context(), "", []string{client.Runtime, "rm", "-f", "-v", staleForkName})
					_, _ = client.runCmd(t.Context(), "", []string{client.Runtime, "rmi", "-f", "md-fork-" + ct.Name})
					t.Cleanup(func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
						defer cancel()
						_, _ = client.runCmd(cleanupCtx, "", []string{client.Runtime, "rmi", "-f", "md-fork-" + ct.Name})
					})

					prepareForkCmd := "printf snapshot > /tmp/fork-marker" +
						" && printf 'fork-uncommitted\n' > " + shellQuote(mountedPath+"/README.md")
					if _, err := ct.runCmd(t.Context(), "", ct.SSHCommand(nil, prepareForkCmd)); err != nil {
						t.Fatalf("prepare source for Fork: %v", err)
					}

					var forkStdout, forkStderr strings.Builder
					mounts, err := ct.AgentMounts(slices.Collect(maps.Values(HarnessMounts))...)
					if err != nil {
						t.Fatalf("AgentMounts: %v", err)
					}
					fork, err := ct.Fork(t.Context(), &forkStdout, &forkStderr, &ForkOpts{
						Quiet:   true,
						Mounts:  mounts,
						MaxCPUs: DefaultMaxCPUs(),
					})
					if err != nil {
						state, _ := client.runCmd(t.Context(), "", []string{client.Runtime, "inspect", "--format", "{{json .State}}", staleForkName})
						logs, _ := client.runCmd(t.Context(), "", []string{client.Runtime, "logs", staleForkName})
						sshDiag, _ := client.runCmd(t.Context(), "", []string{client.Runtime, "exec", staleForkName, "bash", "-lc", strings.Join([]string{
							"stat -c '%U:%G %a %n' /home/user /home/user/.ssh /home/user/.ssh/authorized_keys /etc/ssh/ssh_host_ed25519_key 2>&1",
							"pgrep -a sshd 2>&1 || true",
							"tail -120 /var/log/auth.log 2>&1 || true",
							"/usr/sbin/sshd -T 2>&1 | head -80 || true",
						}, "; ")})
						t.Fatalf("Fork: %v\nstdout:\n%s\nstderr:\n%s\nstate:\n%s\nlogs:\n%s\nssh diagnostics:\n%s", err, forkStdout.String(), forkStderr.String(), state, logs, sshDiag)
					}
					t.Cleanup(func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
						defer cancel()
						if err := fork.Purge(cleanupCtx, io.Discard, io.Discard); err != nil {
							t.Logf("cleanup %s: %v", fork.Name, err)
						}
					})

					if _, err := client.runCmd(t.Context(), "", []string{client.Runtime, "image", "inspect", "md-fork-" + ct.Name}); err == nil {
						t.Fatalf("temporary fork snapshot tag md-fork-%s still exists", ct.Name)
					}

					if len(fork.Repos) != 1 {
						t.Fatalf("fork repos = %d, want 1", len(fork.Repos))
					}
					if fork.Repos[0].Branch != "main-0" {
						t.Fatalf("fork branch = %q, want main-0", fork.Repos[0].Branch)
					}
					out, err := fork.runCmd(t.Context(), "", fork.SSHCommand(nil, "cat /tmp/fork-marker && printf '\n' && git -C "+shellQuote(mountedPath)+" branch --show-current && cat "+shellQuote(mountedPath+"/README.md")))
					if err != nil {
						t.Fatalf("inspect fork: %v", err)
					}
					for _, want := range []string{"snapshot", "main-0", "fork-uncommitted"} {
						if !strings.Contains(out, want) {
							t.Fatalf("fork output missing %q:\n%s", want, out)
						}
					}
				})
			})

			t.Run("run_apply_patch", func(t *testing.T) {
				repo := createSmokeGitRepo(t, "main", "main", false)
				runSmokeMD(t, client,
					"run",
					"-image", baseImage,
					"-repo", repo,
					"-branch", "main",
					"-no-caches",
					"-apply-patch",
					"bash", "-c", "echo foo > bar.txt",
				)
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
				mountedPath := "/home/user/src/smoke-" + rt + "-rebase"
				ct := launchSmokeRepoContainer(t, t.Context(), client, baseImage, &Repo{
					GitRoot:     repo,
					Branch:      "main",
					MountedPath: mountedPath,
				})

				prepareRebaseCmd := strings.Join([]string{
					"cd " + shellQuote(mountedPath),
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
				mountedPath := "/home/user/src/smoke-" + rt + "-non-default-base"
				ct := launchSmokeRepoContainer(t, t.Context(), client, baseImage, &Repo{
					GitRoot:     repo,
					Branch:      "feature",
					MountedPath: mountedPath,
				})

				releaseCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/upstream/release")
				upstreamFeatureCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "refs/remotes/upstream/feature")
				localFeatureCommit := runSmokeGit(t, t.Context(), repo, "rev-parse", "feature")
				assertSmokeContainerGitRef(t, ct, mountedPath, "refs/remotes/upstream/release", releaseCommit)
				assertSmokeContainerGitRef(t, ct, mountedPath, "refs/remotes/upstream/feature", upstreamFeatureCommit)
				assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/host/release")
				assertSmokeContainerGitRef(t, ct, mountedPath, "refs/remotes/host/feature", localFeatureCommit)
				assertSmokeContainerGitRef(t, ct, mountedPath, "feature", localFeatureCommit)
				assertSmokeContainerGitRefMissing(t, ct, mountedPath, "base")
				if upstreamFeatureCommit == localFeatureCommit {
					t.Fatal("test setup error: upstream feature equals local feature; expected an unpushed commit")
				}
				assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/upstream/HEAD")
				assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/origin/release")
				assertSmokeContainerGitRefMissing(t, ct, mountedPath, "refs/remotes/host/HEAD")
			})

			if isRootlessPodman(rt) {
				t.Log("pruning unused specialized images before cache test ...")
				if _, err := client.PruneImages(t.Context(), io.Discard, io.Discard); err != nil {
					t.Fatalf("PruneImages: %v", err)
				}
			}

			// Cache subtest: creates a different specialized image (with cache
			// mounts). Rootless podman may need an ID-mapped copy of the large
			// base layers for each specialized image, so prune the now-unused
			// no-cache image above before creating the cache image.
			t.Run("cache", func(t *testing.T) {
				t.Parallel()
				src := t.TempDir()
				if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("cache-works"), 0o600); err != nil {
					t.Fatal(err)
				}
				ct := launchSmokeContainer(t, t.Context(), client, baseImage, rt+"-cache", false, CacheMount{
					Name:          "smoke-cache",
					HostPath:      src,
					ContainerPath: "/home/user/.cache/smoke",
				})

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
						_, _ = client.runCmd(subCtx, "", []string{rt, "rmi", "-f", img})
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
					out, err := client.runCmd(subCtx, "", []string{
						rt, "image", "inspect", "--format",
						fmt.Sprintf("{{index .Config.Labels %q}}", label), "md-user-local",
					})
					if err != nil {
						t.Errorf("inspecting label %s: %v", label, err)
					} else if out == "" || out == "<no value>" {
						t.Errorf("label %s missing from md-user-local", label)
					}
				}
			})
		})
	}
}
