// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for the md client package.

package md

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestSanitizeDockerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "simple", "simple"},
		{"slash", "with/slash", "with-slash"},
		{"at", "with@at", "with-at"},
		{"feature_branch", "feature/my-branch", "feature-my-branch"},
		{"double_slash", "a//b", "a-b"},
		{"only_dashes", "---", "unnamed"},
		{"empty", "", "unnamed"},
		{"bang", "hello world!", "helloworld"},
		{"leading_dot", ".leading", "leading"},
		{"trailing_dot", "trailing.", "trailing"},
		{"collapse", "a--b..c__d", "a-b-c-d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeDockerName(tt.in); got != tt.want {
				t.Errorf("sanitizeDockerName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestContainerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		repo, branch string
		want         string
	}{
		{"simple", "myrepo", "main", "md-myrepo-main"},
		{"slashes", "my/repo", "feature/branch", "md-my-repo-feature-branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containerName(tt.repo, tt.branch); got != tt.want {
				t.Errorf("containerName(%q, %q) = %q, want %q", tt.repo, tt.branch, got, tt.want)
			}
		})
	}
}

func TestHarnessMounts(t *testing.T) {
	t.Parallel()
	if len(HarnessMounts) == 0 {
		t.Fatal("HarnessMounts must not be empty")
	}
	for harness, paths := range HarnessMounts {
		if paths.Description == "" {
			t.Errorf("HarnessMounts[%q]: Description is empty", harness)
		}
	}
}

func TestWellKnownCaches(t *testing.T) {
	t.Parallel()
	if len(WellKnownCaches) == 0 {
		t.Fatal("WellKnownCaches must not be empty")
	}
	for name, mounts := range WellKnownCaches {
		if len(mounts) == 0 {
			t.Errorf("WellKnownCaches[%q] is empty", name)
		}
		for _, m := range mounts {
			if m.Name == "" {
				t.Errorf("WellKnownCaches[%q]: CacheMount.Name is empty", name)
			}
			if !strings.HasPrefix(m.HostPath, "~/") {
				t.Errorf("WellKnownCaches[%q] %q: HostPath should start with ~/; got %q", name, m.Name, m.HostPath)
			}
			if !strings.HasPrefix(m.ContainerPath, "/home/user/") {
				t.Errorf("WellKnownCaches[%q] %q: ContainerPath should start with /home/user/; got %q", name, m.Name, m.ContainerPath)
			}
		}
		if mounts[0].Description == "" {
			t.Errorf("WellKnownCaches[%q]: first CacheMount.Description is empty", name)
		}
	}
}

func TestClient(t *testing.T) {
	t.Parallel()
	t.Run("Container", func(t *testing.T) {
		t.Parallel()
		c := &Client{}
		tests := []struct {
			name     string
			gitRoot  string
			wantRepo string
			wantName string
		}{
			{"regular", "/home/user/src/myrepo", "/home/user/src/myrepo", "md-myrepo-main"},
			{"bare", "/home/user/src/myrepo.git", "/home/user/src/myrepo", "md-myrepo-main"},
			{"no_git_suffix", "/home/user/src/myrepo.git.git", "/home/user/src/myrepo.git", "md-myrepo.git-main"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				ct, err := c.Container(Repo{GitRoot: tt.gitRoot, Branch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				if ct.Repos[0].MountedPath != tt.wantRepo {
					t.Errorf("MountedPath = %q, want %q", ct.Repos[0].MountedPath, tt.wantRepo)
				}
				if ct.Name != tt.wantName {
					t.Errorf("Name = %q, want %q", ct.Name, tt.wantName)
				}
			})
		}
	})
	t.Run("Runtime", func(t *testing.T) {
		t.Parallel()
		t.Run("new_defaults_to_docker", func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			c, err := newClient(tmp, "", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.Runtime == "" {
				t.Error("New() should set Runtime")
			}
		})
		t.Run("explicit", func(t *testing.T) {
			t.Parallel()
			c := &Client{Runtime: "podman"}
			if c.Runtime != "podman" {
				t.Errorf("Runtime = %q, want %q", c.Runtime, "podman")
			}
		})
	})
}

func TestDetectRuntime(t *testing.T) {
	t.Parallel()
	t.Run("fallback_to_docker", func(t *testing.T) {
		t.Parallel()
		lookPath := func(name string) (string, error) {
			return "", exec.ErrNotFound
		}
		if got := detectRuntime(lookPath); got != "docker" {
			t.Errorf("detectRuntime() = %q, want %q (fallback)", got, "docker")
		}
	})
	t.Run("finds_podman_when_no_docker", func(t *testing.T) {
		t.Parallel()
		lookPath := func(name string) (string, error) {
			if name == "podman" {
				return "/usr/bin/podman", nil
			}
			return "", exec.ErrNotFound
		}
		if got := detectRuntime(lookPath); got != "podman" {
			t.Errorf("detectRuntime() = %q, want %q", got, "podman")
		}
	})
}

func TestIsRootlessPodman(t *testing.T) {
	t.Parallel()
	t.Run("docker", func(t *testing.T) {
		t.Parallel()
		if isRootlessPodman("docker") {
			t.Error("isRootlessPodman(\"docker\") = true, want false")
		}
	})
	t.Run("podman", func(t *testing.T) {
		t.Parallel()
		got := isRootlessPodman("podman")
		if runtime.GOOS == "linux" {
			// On Linux, result depends on whether tests run as root.
			want := os.Getuid() != 0
			if got != want {
				t.Errorf("isRootlessPodman(\"podman\") = %v, want %v (uid=%d)", got, want, os.Getuid())
			}
		} else if got {
			t.Error("isRootlessPodman(\"podman\") = true on non-Linux, want false")
		}
	})
}

func TestRscFS(t *testing.T) {
	t.Parallel()
	t.Run("root_Dockerfile", func(t *testing.T) {
		t.Parallel()
		if _, err := rscFS.ReadFile("rsc/root/Dockerfile"); err != nil {
			t.Fatalf("embedded rsc/root/Dockerfile not found: %v", err)
		}
	})
	t.Run("user_Dockerfile", func(t *testing.T) {
		t.Parallel()
		if _, err := rscFS.ReadFile("rsc/user/Dockerfile"); err != nil {
			t.Fatalf("embedded rsc/user/Dockerfile not found: %v", err)
		}
	})
}
