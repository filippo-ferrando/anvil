//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
)

func TestFCJailLayout(t *testing.T) {
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, "instances", id) }
	t.Cleanup(func() { instanceDir = orig })
	if err := os.MkdirAll(instanceDir("J1"), 0o750); err != nil {
		t.Fatal(err)
	}

	plain := fcPathsFor("J1", nil)
	if filepath.Dir(plain.APISocket) != instanceDir("J1") || plain.InVMM(plain.vsock) != plain.vsock {
		t.Fatalf("unjailed paths = %+v", plain)
	}

	jail := &firecracker.Jail{ID: "J1", ChrootBase: filepath.Join(root, "jailer")}
	paths := fcPathsFor("J1", jail)
	if filepath.Dir(paths.APISocket) != jail.Root() || paths.ConsoleLog != plain.ConsoleLog {
		t.Fatalf("jailed paths = %+v", paths)
	}
	if err := prepareJail("J1", paths); err != nil {
		t.Fatal(err)
	}
	// Firecracker's log written in the jail lands in the instance dir, which outlives it.
	if err := os.WriteFile(paths.LogFile, []byte("log line\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(instanceDir("J1"), "firecracker.log")); string(data) != "log line\n" {
		t.Fatalf("instance log = %q", data)
	}

	disk := filepath.Join(instanceDir("J1"), "disk.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o640); err != nil {
		t.Fatal(err)
	}
	vmDisk, err := jailFile(paths, disk, "disk.raw", false)
	if err != nil || vmDisk != "/disk.raw" {
		t.Fatalf("jailFile = %q, %v", vmDisk, err)
	}
	a, _ := os.Stat(disk)
	b, _ := os.Stat(filepath.Join(jail.Root(), "disk.raw"))
	if !os.SameFile(a, b) {
		t.Fatal("the disk in the jail isn't a hard link to the instance's disk")
	}

	removeJail(paths)
	if _, err := os.Stat(filepath.Dir(jail.Root())); !os.IsNotExist(err) {
		t.Fatal("removeJail left the jail dir")
	}
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("removeJail took the disk with it: %v", err)
	}
}
