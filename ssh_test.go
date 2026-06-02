// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for ssh.go

package md

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureSSHConfigInclude(t *testing.T) {
	t.Parallel()
	t.Run("writes_relative_include", func(t *testing.T) {
		t.Parallel()
		sshDir := filepath.Join(t.TempDir(), ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := ensureSSHConfigInclude(io.Discard, sshDir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(sshDir, "config")) //nolint:gosec // path is under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		want := "Include config.d/*.conf"
		if !strings.Contains(string(data), want) {
			t.Fatalf("config = %q, want containing %q", data, want)
		}
	})

	t.Run("accepts_relative_include", func(t *testing.T) {
		t.Parallel()
		sshDir := filepath.Join(t.TempDir(), ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(sshDir, "config")
		if err := os.WriteFile(configPath, []byte("Include config.d/*.conf\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureSSHConfigInclude(io.Discard, sshDir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(configPath) //nolint:gosec // path is under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		want := "Include config.d/*.conf"
		if string(data) != want+"\n" {
			t.Fatalf("config = %q, want %q", data, want+"\n")
		}
	})

	t.Run("accepts_absolute_include", func(t *testing.T) {
		t.Parallel()
		sshDir := filepath.Join(t.TempDir(), ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		absoluteInclude := "Include " + filepath.ToSlash(filepath.Join(sshDir, "config.d", "*.conf"))
		configPath := filepath.Join(sshDir, "config")
		if err := os.WriteFile(configPath, []byte(absoluteInclude+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureSSHConfigInclude(io.Discard, sshDir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(configPath) //nolint:gosec // path is under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != absoluteInclude+"\n" {
			t.Fatalf("config = %q, want %q", data, absoluteInclude+"\n")
		}
	})
}
