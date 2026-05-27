// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	return os.WriteFile(path+".pub", []byte(pubLine), 0o644) //nolint:gosec // public key is intentionally world-readable
}

// ensurePublicKey regenerates the .pub file from the private key if missing.
func ensurePublicKey(privPath string) error {
	pubPath := privPath + ".pub"
	if _, err := os.Stat(pubPath); err == nil {
		return nil
	}
	privBytes, err := os.ReadFile(privPath) //nolint:gosec // privPath is from trusted config
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(privBytes)
	if err != nil {
		return fmt.Errorf("parsing private key %s: %w", privPath, err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return os.WriteFile(pubPath, []byte(pubLine), 0o600) //nolint:gosec // path is constructed from trusted key dir
}

// controlSocketPath returns the ControlMaster socket path for a container.
func controlSocketPath(containerName string) string {
	return filepath.Join(os.TempDir(), "md-"+containerName+".sock")
}

// writeSSHConfig writes the SSH config file for a container.
// When controlMaster is true, ControlMaster/ControlPath/ControlPersist
// directives are included for connection multiplexing.
func writeSSHConfig(configDir, containerName string, port int32, identityFile, knownHostsFile string, controlMaster bool) error {
	confPath := filepath.Join(configDir, containerName+".conf")
	content := fmt.Sprintf(
		"Host %s\n"+
			"  HostName 127.0.0.1\n"+
			"  Port %d\n"+
			"  User user\n"+
			"  IdentityFile %s\n"+
			"  IdentitiesOnly yes\n"+
			"  UserKnownHostsFile %s\n"+
			"  StrictHostKeyChecking yes\n"+
			"  AddressFamily inet\n"+
			"  GSSAPIAuthentication no\n"+
			"  ConnectTimeout 5\n"+
			"  PreferredAuthentications publickey\n",
		containerName, port, identityFile, knownHostsFile)
	if controlMaster {
		content += fmt.Sprintf(
			"  ControlMaster auto\n"+
				"  ControlPath %s\n"+
				"  ControlPersist 5s\n",
			controlSocketPath(containerName))
	}
	return os.WriteFile(confPath, []byte(content), 0o600)
}

// writeKnownHosts writes the known hosts file for a container.
func writeKnownHosts(knownHostsPath string, port int32, hostPubKey string) error {
	content := fmt.Sprintf("[127.0.0.1]:%d %s\n", port, hostPubKey)
	return os.WriteFile(knownHostsPath, []byte(content), 0o600) //nolint:gosec // path is constructed from trusted config dir
}

// ensureSSHConfigInclude ensures ~/.ssh/config contains an Include directive
// for config.d/*.conf. When the config file doesn't exist, it is created.
// When it exists but the directive is missing, a warning is printed.
func ensureSSHConfigInclude(w io.Writer, sshDir string) error {
	configPath := filepath.Join(sshDir, "config")
	needle := "Include config.d/*.conf"
	data, err := os.ReadFile(configPath) //nolint:gosec // configPath is from trusted SSH dir
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == needle {
			return nil
		}
	}
	if len(data) == 0 {
		content := "# Load all configuration files in config.d/.\n" + needle + "\n"
		return os.WriteFile(configPath, []byte(content), 0o600)
	}
	_, _ = fmt.Fprintf(w, "WARNING: %s is missing the Include directive for per-container SSH configs.\n", configPath)
	_, _ = fmt.Fprintf(w, "  Consider adding the following line at the top of %s:\n", configPath)
	_, _ = fmt.Fprintf(w, "    %s\n", needle)
	return nil
}

// removeSSHConfig removes SSH config and known_hosts files for a container.
// It also closes any active ControlMaster connection and removes the socket.
func removeSSHConfig(ctx context.Context, configDir, containerName string) {
	cleanupControlSocket(ctx, containerName)
	_ = os.Remove(filepath.Join(configDir, containerName+".conf"))
	_ = os.Remove(filepath.Join(configDir, containerName+".known_hosts"))
}

// cleanupControlSocket closes an active ControlMaster connection and removes
// the socket file. Safe to call even when ControlMaster is not in use.
func cleanupControlSocket(ctx context.Context, containerName string) {
	sock := controlSocketPath(containerName)
	if _, err := os.Stat(sock); err != nil {
		return
	}
	_ = exec.CommandContext(ctx, "ssh", "-O", "exit", "-S", sock, "x").Run() //nolint:gosec // sock is from trusted container name
	_ = os.Remove(sock)
}
