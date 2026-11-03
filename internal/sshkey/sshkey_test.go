package sshkey

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEnsureDefaultGeneratesAndReuses(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH in this environment")
	}

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := EnsureDefault()
	if err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("expected path under %s, got %s", dir, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected private key to exist: %v", err)
	}
	if _, err := os.Stat(path + ".pub"); err != nil {
		t.Fatalf("expected public key to exist: %v", err)
	}

	// Calling again must reuse the same key, not regenerate it.
	again, err := EnsureDefault()
	if err != nil {
		t.Fatalf("second EnsureDefault: %v", err)
	}
	if again != path {
		t.Fatalf("expected the same path on reuse, got %s and %s", path, again)
	}
}

func TestEnsureDefaultPublicIsTrimmed(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH in this environment")
	}

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	pub, err := EnsureDefaultPublic()
	if err != nil {
		t.Fatalf("EnsureDefaultPublic: %v", err)
	}
	if pub == "" || strings.ContainsAny(pub, "\n\r") {
		t.Fatalf("expected a trimmed single-line public key, got %q", pub)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("expected an ed25519 public key, got %q", pub)
	}
}

// A private key left without its .pub (crash between the two links) gets its .pub rebuilt.
func TestEnsureRebuildsMissingPublic(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH in this environment")
	}
	path := t.TempDir() + "/id_ed25519"
	if err := Ensure(path, "anvil"); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path + ".pub")
	if err := os.Remove(path + ".pub"); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(path, "anvil"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatalf("public key not rebuilt: %v", err)
	}
	key := func(b []byte) string { return strings.Join(strings.Fields(string(b))[:2], " ") }
	if key(got) != key(want) {
		t.Fatalf("rebuilt key %q, want %q", got, want)
	}
}
