// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

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
	t.Run("writes_absolute_include", func(t *testing.T) {
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
		want := "Include " + filepath.ToSlash(filepath.Join(sshDir, "config.d", "*.conf"))
		if !strings.Contains(string(data), want) {
			t.Fatalf("config = %q, want containing %q", data, want)
		}
	})

	t.Run("migrates_relative_include", func(t *testing.T) {
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
		if strings.Contains(string(data), "Include config.d/*.conf") {
			t.Fatalf("relative include was not migrated:\n%s", data)
		}
		want := "Include " + filepath.ToSlash(filepath.Join(sshDir, "config.d", "*.conf"))
		if !strings.Contains(string(data), want) {
			t.Fatalf("config = %q, want containing %q", data, want)
		}
	})
}
