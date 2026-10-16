package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// TestDeletePurgeRemovesUnusedBaseImage covers --purge: the cached base image stays
// while another VM's disk sits on it, and goes with the last one.
func TestDeletePurgeRemovesUnusedBaseImage(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}
	dir := t.TempDir()
	vault := image.NewVault(filepath.Join(dir, "images"))
	if err := os.MkdirAll(vault.PreparedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(vault.PreparedDir, "test-x86_64.qcow2")
	qemuImg(t, "create", "-f", "qcow2", base, "16M")

	s := newTestServer(t, readyBackend{})
	s.Vault = vault
	for _, name := range []string{"a", "b"} {
		disk := filepath.Join(dir, name+".qcow2")
		qemuImg(t, "create", "-f", "qcow2", "-F", "qcow2", "-b", base, disk)
		params := instance.LaunchParams{Name: name, Kind: instance.KindVM, VM: &instance.VMSpec{DiskPath: disk}, NoStart: true}
		if err := s.Manager.Launch(context.Background(), params, func(instance.LaunchEvent) {}); err != nil {
			t.Fatal(err)
		}
	}

	purge := func(name string) {
		t.Helper()
		if _, err := s.Delete(context.Background(), &anvilv1.DeleteRequest{Names: []string{name}, Purge: true}); err != nil {
			t.Fatal(err)
		}
	}
	purge("a")
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base image removed while b still uses it: %v", err)
	}
	purge("b")
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("base image should be gone with its last user, stat: %v", err)
	}
}

func qemuImg(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("qemu-img", args...).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img %v: %v: %s", args, err, out)
	}
}
