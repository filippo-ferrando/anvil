// Package sshkey manages anvil's own default guest-access SSH keypair —
// shared by every client entry point that needs to reach a VM's guest
// (the CLI's shell/exec/transfer/launch commands, and the TUI, M8),
// pulled out of internal/cli/commands so neither has its own copy to
// drift out of sync with the other.
package sshkey

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultPath is anvil's own managed SSH keypair (private key path; the
// public half is the same path + ".pub"). This is deliberately separate
// from any personal ~/.ssh key the user has for other purposes — the same
// idea behind Vagrant's well-known shared "insecure" keypair and
// Multipass's own managed key: this key isn't protecting anything beyond
// "don't let an unrelated local process into the VM" (SLIRP host-forwarding
// is only ever reachable from this same host to begin with), so a
// passphrase would only add friction with no real security benefit, and
// tying VM access to a user's actual personal identity key is more coupling
// than this needs.
func DefaultPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding a config directory: %w", err)
	}
	return filepath.Join(configDir, "anvil", "ssh", "id_ed25519"), nil
}

// EnsureDefault returns the private key path, generating a fresh
// passphrase-less ed25519 keypair there via `ssh-keygen` (already a
// dependency alongside `ssh`/`scp`) the first time it's needed.
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
	cmd := exec.Command(keygenBin, "-t", "ed25519", "-N", "", "-C", "anvil", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("generating anvil's SSH key: %w: %s", err, out)
	}
	return path, nil
}

// EnsureDefaultPublic is EnsureDefault, returning the trimmed public key
// text instead of the private key's path — what a caller actually wants
// to bake into a VM's authorized_keys or print for someone else to.
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
