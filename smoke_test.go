// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build smoke

package md

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hasImage checks whether a container image exists in the local store.
func hasImage(ctx context.Context, rt, name string) bool {
	_, err := runCmd(ctx, "", []string{rt, "image", "inspect", "--format", "{{.Id}}", name})
	return err == nil
}

// ensureImages ensures md-root-local and md-user-local exist. When they don't
// and -short is passed, falls back to the remote default image instead of
// building. Returns the base image to use.
func ensureImages(t *testing.T, ctx context.Context, rt string) string {
	if hasImage(ctx, rt, "md-root-local") && hasImage(ctx, rt, "md-user-local") {
		t.Log("local images already present, skipping build")
		return "md-user-local"
	}
	if testing.Short() {
		t.Log("short mode: using remote image " + DefaultBaseImage)
		return DefaultBaseImage + ":latest"
	}
	t.Log("building local images (md-root-local → md-user-local) ...")
	client, err := New(io.Discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.Runtime = rt
	if err := client.BuildImage(ctx, io.Discard, io.Discard); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	t.Log("images built successfully")
	return "md-user-local"
}

// launchSmokeContainer creates a Client, allocates a container with the given
// name suffix, cleans up leftovers, and calls Launch+Connect with -sudo.
// Returns the live container (caller must Purge via t.Cleanup).
func launchSmokeContainer(t *testing.T, ctx context.Context, rt, baseImage, nameSuffix string, caches ...CacheMount) *Container {
	client, err := New(io.Discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.Runtime = rt

	ct, err := client.Container()
	if err != nil {
		t.Fatalf("Container: %v", err)
	}
	ct.Name = "md-smoke-" + nameSuffix

	_, _ = runCmd(ctx, "", []string{rt, "rm", "-f", "-v", ct.Name})

	opts := &StartOpts{
		BaseImage: baseImage,
		Sudo:      true,
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

// TestSmoke verifies end-to-end: build images, start a sudo-enabled container,
// confirm rootless podman works inside, pull from registries, and exercise the
// container lifecycle. Runs under each available container runtime.
func TestSmoke(t *testing.T) {
	// Use a temporary home so the test doesn't depend on ~/.config/md
	// being writable (e.g. when running inside a read-only container).
	// os.MkdirTemp + manual cleanup (not t.TempDir) so that rootless
	// podman storage permission errors on cleanup don't fail the test.
	tmpHome, err := os.MkdirTemp("", "md-smoke-home-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpHome) })
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))

	for _, rt := range []string{"docker", "podman"} {
		t.Run(rt, func(t *testing.T) {
			if _, err := exec.LookPath(rt); err != nil {
				t.Skipf("skipping: %s not in PATH", rt)
			}
			if rt == "podman" && os.Getuid() == 0 {
				t.Skip("skipping: rootless podman smoke test requires non-root user")
			}

			// Rootless podman adds --userns=keep-id, which puts the
			// inner container in a user namespace. Nested newuidmap
			// then fails with EPERM — user namespace stacking is not
			// supported. Rootful docker and rootful podman (uid 0)
			// don't have this limitation.
			// Error: "newuidmap: write to uid_map failed: Operation not permitted"
			nestedOK := rt != "podman" || os.Getuid() == 0

			baseImage := ensureImages(t, t.Context(), rt)

			t.Run("launch", func(t *testing.T) {
				ct := launchSmokeContainer(t, t.Context(), rt, baseImage, rt+"-launch")

				t.Run("sudo", func(t *testing.T) {
					out, err := runCmd(t.Context(), "", ct.SSHCommand(ct.Name, "echo '"+ct.sudoPassword+"' | sudo -S whoami"))
					if err != nil {
						t.Fatalf("sudo whoami: %v", err)
					}
					if got := strings.TrimSpace(out); got != "root" {
						t.Fatalf("sudo whoami expected 'root', got %q", got)
					}
					t.Log("sudo works inside the container")
				})

				t.Run("file_io", func(t *testing.T) {
					if _, err := runCmd(t.Context(), "", ct.SSHCommand(ct.Name, "echo hello > /tmp/smoke-test && cat /tmp/smoke-test")); err != nil {
						t.Fatalf("file I/O: %v", err)
					}
				})
			})

			t.Run("cache", func(t *testing.T) {
				src := t.TempDir()
				if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("cache-works"), 0o644); err != nil {
					t.Fatal(err)
				}
				ct := launchSmokeContainer(t, t.Context(), rt, baseImage, rt+"-cache", CacheMount{
					Name:          "smoke-cache",
					HostPath:      src,
					ContainerPath: "/home/user/.cache/smoke",
				})

				out, err := runCmd(t.Context(), "", ct.SSHCommand(ct.Name, "cat /home/user/.cache/smoke/hello.txt"))
				if err != nil {
					t.Fatalf("read cached file: %v", err)
				}
				if got := strings.TrimSpace(out); got != "cache-works" {
					t.Fatalf("expected 'cache-works', got %q", got)
				}
				t.Log("cache injection works")
			})

			t.Run("nested", func(t *testing.T) {
				if !nestedOK {
					t.Skip("skipping: nested newuidmap fails with rootless podman (user namespace stacking)")
				}
				ct := launchSmokeContainer(t, t.Context(), rt, baseImage, rt+"-nested")

				t.Run("version", func(t *testing.T) {
					args := ct.SSHCommand(ct.Name, "podman version --format '{{.Version}}'")
					out, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput()
					if err != nil {
						t.Fatalf("podman version: %v\noutput: %s", err, string(out))
					}
					if got := strings.TrimSpace(string(out)); got == "" {
						t.Fatal("podman returned empty version")
					} else {
						t.Logf("nested podman version: %s", got)
					}
				})

				t.Run("info", func(t *testing.T) {
					args := ct.SSHCommand(ct.Name, "podman info --format '{{.Host.RemoteSocket.Path}}'")
					out, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput()
					if err != nil {
						t.Fatalf("podman info: %v\noutput: %s", err, string(out))
					}
					t.Logf("nested podman socket: %s", strings.TrimSpace(string(out)))
				})

				t.Run("run_alpine", func(t *testing.T) {
					subCtx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
					defer cancel()
					args := ct.SSHCommand(ct.Name, "podman run --rm docker.io/alpine:latest echo hello-from-nested-podman")
					out, err := exec.CommandContext(subCtx, args[0], args[1:]...).Output()
					if err != nil {
						var exitErr *exec.ExitError
						if errors.As(err, &exitErr) {
							t.Fatalf("podman run alpine: %v\nstderr: %s", err, string(exitErr.Stderr))
						}
						t.Fatalf("podman run alpine: %v", err)
					}
					if got := strings.TrimSpace(string(out)); got != "hello-from-nested-podman" {
						t.Fatalf("expected 'hello-from-nested-podman', got %q", got)
					}
					t.Logf("nested podman run: %s", strings.TrimSpace(string(out)))
				})

				t.Run("run_alpine_id", func(t *testing.T) {
					subCtx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
					defer cancel()
					out, err := runCmd(subCtx, "", ct.SSHCommand(ct.Name, "podman run --rm docker.io/alpine:latest id -u"))
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
					args := ct.SSHCommand(ct.Name, "podman pull docker.io/busybox:latest")
					out, err := exec.CommandContext(subCtx, args[0], args[1:]...).CombinedOutput()
					if err != nil {
						t.Fatalf("podman pull busybox: %v\noutput: %s", err, string(out))
					}
					t.Logf("pull output: %s", strings.TrimSpace(string(out)))
				})

				t.Run("run_busybox", func(t *testing.T) {
					subCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					args := ct.SSHCommand(ct.Name, "podman run --rm docker.io/busybox:latest echo ok")
					out, err := exec.CommandContext(subCtx, args[0], args[1:]...).CombinedOutput()
					if err != nil {
						t.Fatalf("podman run busybox: %v\noutput: %s", err, string(out))
					}
					if got := strings.TrimSpace(string(out)); got != "ok" {
						t.Fatalf("expected 'ok', got %q", got)
					}
				})
			})

			t.Run("lifecycle", func(t *testing.T) {
				ct := launchSmokeContainer(t, t.Context(), rt, baseImage, rt+"-lifecycle")

				if _, err := runCmd(t.Context(), "", ct.SSHCommand(ct.Name, "echo persisted > /tmp/smoke-test")); err != nil {
					t.Fatalf("write file: %v", err)
				}

				t.Log("stopping container ...")
				if err := ct.Stop(t.Context()); err != nil {
					t.Fatalf("Stop: %v", err)
				}

				t.Log("reviving container ...")
				if err := ct.Revive(t.Context(), io.Discard, io.Discard); err != nil {
					t.Fatalf("Revive: %v", err)
				}

				out, err := runCmd(t.Context(), "", ct.SSHCommand(ct.Name, "cat /tmp/smoke-test"))
				if err != nil {
					t.Fatalf("read file after revive: %v", err)
				}
				if got := strings.TrimSpace(out); got != "persisted" {
					t.Fatalf("expected 'persisted', got %q", got)
				}
			})

			t.Run("build_image", func(t *testing.T) {
				if testing.Short() {
					t.Skip("skipping: clean rebuild in short mode")
				}
				subCtx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
				defer cancel()

				for _, img := range []string{"md-root-local", "md-user-local"} {
					if hasImage(subCtx, rt, img) {
						t.Logf("removing existing image %s for clean build test", img)
						_, _ = runCmd(subCtx, "", []string{rt, "rmi", "-f", img})
					}
				}

				client, err := New(io.Discard)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				client.Runtime = rt

				t.Log("building images from scratch ...")
				if err := client.BuildImage(subCtx, os.Stdout, os.Stderr); err != nil {
					t.Fatalf("BuildImage: %v", err)
				}

				for _, img := range []string{"md-root-local", "md-user-local"} {
					if !hasImage(subCtx, rt, img) {
						t.Errorf("image %s not found after build", img)
					}
				}

				labels := []string{
					"org.opencontainers.image.source",
					"org.opencontainers.image.licenses",
				}
				for _, label := range labels {
					out, err := runCmd(subCtx, "", []string{
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
