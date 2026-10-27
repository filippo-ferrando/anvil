package client

import (
	"fmt"
	"os"
	"strings"

	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
)

// ResolveSSHKeys returns anvil's own managed public key, generated on first use,
// plus each extra entry: a literal public key or a path to a .pub file.
func ResolveSSHKeys(extra []string) ([]string, error) {
	anvilPub, err := sshkey.EnsureDefaultPublic()
	if err != nil {
		return nil, err
	}
	keys := []string{anvilPub}
	for _, k := range extra {
		resolved, err := resolveSSHKey(k)
		if err != nil {
			return nil, err
		}
		keys = append(keys, resolved)
	}
	return keys, nil
}

// resolveSSHKey reads k as a literal key when it starts with a key-type prefix, else as a .pub file path.
func resolveSSHKey(k string) (string, error) {
	for _, prefix := range []string{"ssh-", "ecdsa-", "sk-"} {
		if strings.HasPrefix(k, prefix) {
			return k, nil
		}
	}
	data, err := os.ReadFile(k)
	if err != nil {
		return "", fmt.Errorf("reading SSH key file %s: %w", k, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ResolveIdentity returns explicit if set, otherwise anvil's own managed
// private key, generated on first use.
func ResolveIdentity(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return sshkey.EnsureDefault()
}
