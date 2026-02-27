// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ensureEd25519Key generates an ed25519 SSH key pair if it doesn't exist.
func ensureEd25519Key(w io.Writer, path, comment string) error {
	if _, err := os.Stat(path); err == nil {
		// Private key exists; ensure public key exists too.
		return ensurePublicKey(path)
	}
	_, _ = fmt.Fprintf(w, "- Generating %s at %s ...\n", comment, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating ed25519 key: %w", err)
	}
	// Marshal private key to OpenSSH format.
	privBytes, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return fmt.Errorf("marshaling private key: %w", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(privBytes), 0o600); err != nil {
		return err
	}
	// Write public key.
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("creating SSH public key: %w", err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(sshPub))
	return os.WriteFile(path+".pub", []byte(pubLine), 0o644)
}

// ensurePublicKey regenerates the .pub file from the private key if missing.
func ensurePublicKey(privPath string) error {
	pubPath := privPath + ".pub"
	if _, err := os.Stat(pubPath); err == nil {
		return nil
	}
	privBytes, err := os.ReadFile(privPath)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(privBytes)
	if err != nil {
		return fmt.Errorf("parsing private key %s: %w", privPath, err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return os.WriteFile(pubPath, []byte(pubLine), 0o644)
}

// writeSSHConfig writes the SSH config file for a container.
func writeSSHConfig(configDir, containerName, port, identityFile, knownHostsFile string) error {
	confPath := filepath.Join(configDir, containerName+".conf")
	content := fmt.Sprintf("Host %s\n  HostName 127.0.0.1\n  Port %s\n  User user\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n",
		containerName, port, identityFile, knownHostsFile)
	return os.WriteFile(confPath, []byte(content), 0o644)
}

// writeKnownHosts writes the known hosts file for a container.
func writeKnownHosts(knownHostsPath, port, hostPubKey string) error {
	content := fmt.Sprintf("[127.0.0.1]:%s %s\n", port, hostPubKey)
	return os.WriteFile(knownHostsPath, []byte(content), 0o644)
}

// ensureSSHConfigInclude ensures ~/.ssh/config contains an Include directive
// for config.d/*.conf. If the directive is missing, it is prepended to the
// file. The config file is created if it doesn't exist.
func ensureSSHConfigInclude(sshDir string) error {
	configPath := filepath.Join(sshDir, "config")
	needle := "Include config.d/*.conf"
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Check whether the Include is already present.
	for line := range strings.SplitSeq(string(data), "\n") {
		// Normalize whitespace for a resilient match.
		if strings.TrimSpace(line) == needle {
			return nil
		}
	}
	// Prepend the Include directive. It must appear before any Host/Match
	// blocks to be effective.
	var buf bytes.Buffer
	buf.WriteString("# Load all configuration files in config.d/.\n")
	buf.WriteString(needle)
	buf.WriteByte('\n')
	if len(data) > 0 {
		buf.WriteByte('\n')
		buf.Write(data)
	}
	return os.WriteFile(configPath, buf.Bytes(), 0o600)
}

// removeSSHConfig removes SSH config and known_hosts files for a container.
func removeSSHConfig(configDir, containerName string) {
	_ = os.Remove(filepath.Join(configDir, containerName+".conf"))
	_ = os.Remove(filepath.Join(configDir, containerName+".known_hosts"))
}
