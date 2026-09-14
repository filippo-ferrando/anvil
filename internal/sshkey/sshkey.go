// Package sshkey manages anvil's default guest-access SSH keypair.
package sshkey

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPath returns anvil's managed private key path; the public half
// is the same path with ".pub" appended.
func DefaultPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding a config directory: %w", err)
	}
	return filepath.Join(configDir, "anvil", "ssh", "id_ed25519"), nil
}

// EnsureDefault returns the private key path, generating a fresh
// passphrase-less ed25519 keypair there the first time it's needed.
func EnsureDefault() (string, error) {
	path, err := DefaultPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking for anvil's SSH key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating anvil's SSH key dir: %w", err)
	}
	keygenBin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: not found on PATH (needed to generate anvil's default SSH key)")
	}

	// Generate under a private temp name, then atomically link into place.
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	defer os.Remove(tmp)
	defer os.Remove(tmp + ".pub")
	cmd := exec.Command(keygenBin, "-t", "ed25519", "-N", "", "-C", "anvil", "-f", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("generating anvil's SSH key: %w: %s", err, out)
	}

	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			// Someone else generated it first — use theirs, discard ours.
			return path, nil
		}
		return "", fmt.Errorf("installing anvil's SSH key: %w", err)
	}
	if err := os.Link(tmp+".pub", path+".pub"); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("installing anvil's SSH public key: %w", err)
	}
	return path, nil
}

// EnsureDefaultPublic returns the trimmed public key text for the
// default keypair, generating it first if needed.
func EnsureDefaultPublic() (string, error) {
	path, err := EnsureDefault()
	if err != nil {
		return "", err
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", fmt.Errorf("reading anvil's default public key: %w", err)
	}
	return strings.TrimSpace(string(pub)), nil
}
