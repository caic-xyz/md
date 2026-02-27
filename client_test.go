// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeDockerName(t *testing.T) {
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
			if got := sanitizeDockerName(tt.in); got != tt.want {
				t.Errorf("sanitizeDockerName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestContainerName(t *testing.T) {
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
			if got := containerName(tt.repo, tt.branch); got != tt.want {
				t.Errorf("containerName(%q, %q) = %q, want %q", tt.repo, tt.branch, got, tt.want)
			}
		})
	}
}

func TestWellKnownCaches(t *testing.T) {
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
	}
}

func TestRuntime(t *testing.T) {
	t.Run("new_defaults_to_docker", func(t *testing.T) {
		c, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if c.Runtime == "" {
			t.Error("New() should set Runtime")
		}
	})
	t.Run("explicit", func(t *testing.T) {
		c := &Client{Runtime: "podman"}
		if c.Runtime != "podman" {
			t.Errorf("Runtime = %q, want %q", c.Runtime, "podman")
		}
	})
}

func TestDetectRuntime(t *testing.T) {
	t.Run("fallback_to_docker", func(t *testing.T) {
		// Use empty PATH to test fallback when neither docker nor podman is found.
		t.Setenv("PATH", t.TempDir())
		if got := detectRuntime(); got != "docker" {
			t.Errorf("detectRuntime() = %q, want %q (fallback)", got, "docker")
		}
	})
	t.Run("finds_podman_when_no_docker", func(t *testing.T) {
		dir := t.TempDir()
		podmanPath := filepath.Join(dir, "podman")
		if err := os.WriteFile(podmanPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		if got := detectRuntime(); got != "podman" {
			t.Errorf("detectRuntime() = %q, want %q", got, "podman")
		}
	})
}

func TestRscFS(t *testing.T) {
	t.Run("Dockerfile", func(t *testing.T) {
		if _, err := rscFS.ReadFile("rsc/Dockerfile"); err != nil {
			t.Fatalf("embedded rsc/Dockerfile not found: %v", err)
		}
	})
	t.Run("Dockerfile.base", func(t *testing.T) {
		if _, err := rscFS.ReadFile("rsc/Dockerfile.base"); err != nil {
			t.Fatalf("embedded rsc/Dockerfile.base not found: %v", err)
		}
	})
}
