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

func TestWriteSSHConfig(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	identityFile := filepath.Join(configDir, "id_ed25519")
	knownHostsFile := filepath.Join(configDir, "known_hosts")
	if err := writeSSHConfig(configDir, "md-test", 2222, identityFile, knownHostsFile, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(configDir, "md-test.conf")) //nolint:gosec // path is under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	want := "Host md-test\n" +
		"  HostName 127.0.0.1\n" +
		"  Port 2222\n" +
		"  User user\n" +
		"  IdentityFile " + filepath.ToSlash(identityFile) + "\n" +
		"  IdentitiesOnly yes\n" +
		"  UserKnownHostsFile " + filepath.ToSlash(knownHostsFile) + "\n" +
		"  StrictHostKeyChecking yes\n" +
		"  AddressFamily inet\n" +
		"  GSSAPIAuthentication no\n" +
		"  ConnectTimeout 5\n" +
		"  PreferredAuthentications publickey\n" +
		"  HostKeyAlgorithms ssh-ed25519\n" +
		"  PubkeyAcceptedAlgorithms ssh-ed25519\n" +
		"  KexAlgorithms curve25519-sha256,curve25519-sha256@libssh.org\n" +
		"  Ciphers aes128-gcm@openssh.com,chacha20-poly1305@openssh.com\n" +
		"  Compression no\n" +
		"  RekeyLimit 16G\n"
	if string(data) != want {
		t.Fatalf("config = %q, want %q", data, want)
	}
}

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
