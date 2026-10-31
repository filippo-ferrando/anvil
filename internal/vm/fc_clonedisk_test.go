//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// fakeDiskHelper puts a plain file in the jail where the root helper would put a device.
type fakeDiskHelper struct {
	err     error
	created []hostdev.CloneDisk
	deleted []string
}

func (f *fakeDiskHelper) CreateCloneDisk(_ context.Context, d hostdev.CloneDisk) error {
	f.created = append(f.created, d)
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(filepath.Join(d.Jail, hostdev.CloneDiskFile), nil, 0o600)
}

func (f *fakeDiskHelper) DeleteCloneDisk(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCloneDisk(t *testing.T) {
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, "instances", id) }
	t.Cleanup(func() { instanceDir = orig })
	warm := filepath.Join(root, "warm", "abc123")
	if err := os.MkdirAll(warm, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(warm, warmDisk), make([]byte, 4096), 0o640); err != nil {
		t.Fatal(err)
	}
	reflinkOK := false
	origReflink := reflinkFile
	reflinkFile = func(src, dst string) error {
		if !reflinkOK {
			return errors.New("not supported")
		}
		return os.Link(src, dst) // stands in for a clone of the file
	}
	t.Cleanup(func() { reflinkFile = origReflink })

	helper := &fakeDiskHelper{}
	b := &FCBackend{Disks: helper}
	clone := func(id string, jailed bool) (string, fcInstancePaths) {
		t.Helper()
		var paths fcInstancePaths
		if jailed {
			paths = fcPathsFor(id, &firecracker.Jail{ID: id, ChrootBase: filepath.Join(root, "jailer"), ExecFile: "/usr/bin/firecracker"})
		} else {
			paths = fcPathsIn(id, nil, filepath.Join(instanceDir(id), "root"))
		}
		for _, dir := range []string{instanceDir(id), paths.root()} {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
		}
		how, err := b.cloneDisk(context.Background(), id, "abc123", warm, paths)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(paths.root(), warmDisk)); err != nil {
			t.Errorf("%s: no disk in the jail: %v", id, err)
		}
		return how, paths
	}

	reflinkOK = true
	if how, _ := clone("eph-R", true); how != diskReflink || len(helper.created) != 0 {
		t.Errorf("with reflinks: %s, helper asked %d times", how, len(helper.created))
	}

	reflinkOK = false
	how, paths := clone("eph-D", true)
	if how != diskDM || len(helper.created) != 1 {
		t.Fatalf("without reflinks: %s, helper asked %d times", how, len(helper.created))
	}
	if d := helper.created[0]; d.ID != "eph-D" || d.Key != "abc123" || d.Jail != paths.root() {
		t.Errorf("helper asked for %+v", d)
	}
	if info, err := os.Stat(filepath.Join(instanceDir("eph-D"), hostdev.CloneCOWFile)); err != nil || info.Size() != 4096 {
		t.Errorf("COW file: %v, %v; want 4096 bytes", info, err)
	}

	// Unjailed clones (tests only) never use the helper.
	if how, _ := clone("eph-U", false); how != diskCopy || len(helper.created) != 1 {
		t.Errorf("unjailed: %s, helper asked %d times", how, len(helper.created))
	}

	// A failing helper falls back to a copy, and isn't asked again.
	helper.err = errors.New("no dm_snapshot")
	for _, id := range []string{"eph-F1", "eph-F2"} {
		if how, _ := clone(id, true); how != diskCopy {
			t.Errorf("%s with a failing helper: %s", id, how)
		}
	}
	if len(helper.created) != 2 {
		t.Errorf("helper asked %d times, want once more after the first failure", len(helper.created))
	}
	if _, err := os.Stat(filepath.Join(instanceDir("eph-F1"), hostdev.CloneCOWFile)); !os.IsNotExist(err) {
		t.Errorf("COW file left after a failure: %v", err)
	}
}

func TestReflink(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("warm disk"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := reflink(src, dst); err != nil {
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Errorf("a failed reflink left %s", dst)
		}
		t.Skipf("no reflinks on %s's filesystem: %v", dir, err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "warm disk" {
		t.Errorf("clone holds %q, %v", data, err)
	}
}

func TestPrivateCopy(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "vmlinux"), filepath.Join(dir, "jail-vmlinux")
	if err := os.WriteFile(src, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := privateCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if data, _ := os.ReadFile(dst); err != nil || string(data) != "kernel" || info.Mode().Perm() != 0o644 {
		t.Errorf("copy holds %q (mode %v, %v), want its own bytes with the source's mode", data, info.Mode(), err)
	}
	if err := privateCopy(src, dst); err == nil {
		t.Error("copied over an existing file")
	}
}

func TestReflinkFork(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("qemu-img", args...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img %v: %v: %s", args, err, out)
		}
	}
	base := filepath.Join(dir, "base.qcow2")
	src := filepath.Join(dir, "src", "disk.qcow2")
	dst := filepath.Join(dir, "dst", "disk.qcow2")
	_ = os.MkdirAll(filepath.Dir(src), 0o750)
	_ = os.MkdirAll(filepath.Dir(dst), 0o750)
	run("create", "-f", "qcow2", base, "64M")
	run("create", "-f", "qcow2", "-F", "qcow2", "-b", base, src)
	run("snapshot", "-c", "before-upgrade", src)
	if err := reflink(src, filepath.Join(dir, "probe")); err != nil {
		t.Skipf("no reflinks on %s's filesystem: %v", dir, err)
	}

	if !reflinkFork(context.Background(), src, dst) {
		t.Fatal("reflinkFork refused a disk with an absolute backing file")
	}
	if backing, err := image.BackingFile(dst); err != nil || backing != base {
		t.Errorf("fork's backing file = %q, %v", backing, err)
	}
	if snaps, err := image.ListSnapshots(dst); err != nil || len(snaps) != 0 {
		t.Errorf("fork kept the source's snapshots: %v, %v", snaps, err)
	}
	if snaps, _ := image.ListSnapshots(src); len(snaps) != 1 {
		t.Errorf("the source lost its snapshot: %v", snaps)
	}
	run("check", dst)

	// A relative backing name would point elsewhere from the fork's dir: convert instead.
	rel := filepath.Join(dir, "src", "rel.qcow2")
	run("create", "-f", "qcow2", "-F", "qcow2", "-b", "../base.qcow2", rel)
	other := filepath.Join(dir, "dst", "rel.qcow2")
	if reflinkFork(context.Background(), rel, other) {
		t.Error("reflinkFork took a disk with a relative backing file")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("a refused fork left %s", other)
	}
}
