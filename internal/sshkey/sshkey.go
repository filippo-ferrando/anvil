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
	return path, Ensure(path, "anvil")
}

// Ensure generates a passphrase-less ed25519 keypair at path unless one is there.
// Concurrent callers are safe: the first key linked into place wins.
func Ensure(path, comment string) error {
	if _, err := os.Stat(path); err == nil {
		return ensurePublic(path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for SSH key %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating SSH key dir: %w", err)
	}
	keygenBin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return fmt.Errorf("ssh-keygen: not found on PATH (needed to generate SSH key %s)", path)
	}

	// Generate under a private temp name, then atomically link into place.
	tmp := tempName(path)
	defer os.Remove(tmp)
	defer os.Remove(tmp + ".pub")
	cmd := exec.Command(keygenBin, "-t", "ed25519", "-N", "", "-C", comment, "-f", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("generating SSH key %s: %w: %s", path, err, out)
	}

	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			// Someone else generated it first: use theirs, but its .pub may not be linked yet.
			return ensurePublic(path)
		}
		return fmt.Errorf("installing SSH key %s: %w", path, err)
	}
	if err := os.Link(tmp+".pub", path+".pub"); err != nil && !os.IsExist(err) {
		return fmt.Errorf("installing SSH public key %s.pub: %w", path, err)
	}
	return nil
}

func tempName(path string) string {
	return fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
}

// ensurePublic rebuilds path.pub from the private key when it is missing,
// e.g. after a crash between linking the two halves.
func ensurePublic(path string) error {
	pubPath := path + ".pub"
	if _, err := os.Stat(pubPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for SSH public key %s: %w", pubPath, err)
	}
	keygenBin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return fmt.Errorf("ssh-keygen: not found on PATH (needed to rebuild %s)", pubPath)
	}
	pub, err := exec.Command(keygenBin, "-y", "-f", path).Output()
	if err != nil {
		return fmt.Errorf("rebuilding SSH public key %s: %w", pubPath, err)
	}
	tmp := tempName(pubPath)
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, pub, 0o644); err != nil {
		return fmt.Errorf("rebuilding SSH public key %s: %w", pubPath, err)
	}
	if err := os.Link(tmp, pubPath); err != nil && !os.IsExist(err) {
		return fmt.Errorf("installing SSH public key %s: %w", pubPath, err)
	}
	return nil
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
