package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSSHKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pub := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 BBBB file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := ResolveSSHKeys([]string{"ecdsa-sha2-nistp256 AAAA lit", pub})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys[1] != "ecdsa-sha2-nistp256 AAAA lit" || keys[2] != "ssh-ed25519 BBBB file" {
		t.Errorf("got %q", keys)
	}
	if _, err := ResolveSSHKeys([]string{filepath.Join(t.TempDir(), "missing.pub")}); err == nil {
		t.Error("expected a missing key file to fail")
	}
}

func TestResolveSSHKeyArgsAddsNoDefaultKey(t *testing.T) {
	keys, err := ResolveSSHKeyArgs([]string{"ssh-ed25519 AAAA lit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "ssh-ed25519 AAAA lit" {
		t.Errorf("got %q", keys)
	}
}

func TestResolveIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got, err := ResolveIdentity("/keys/id"); err != nil || got != "/keys/id" {
		t.Errorf("explicit: got %q, %v", got, err)
	}
	got, err := ResolveIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("expected anvil's own key at %q: %v", got, err)
	}
}
