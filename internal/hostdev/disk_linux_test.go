//go:build linux

package hostdev

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseDevNum(t *testing.T) {
	got, err := parseDevNum(" 254:3\n")
	if err != nil || unix.Major(got) != 254 || unix.Minor(got) != 3 {
		t.Errorf("parseDevNum = %d:%d, %v", unix.Major(got), unix.Minor(got), err)
	}
	for _, bad := range []string{"", "254", "a:b", "254:"} {
		if _, err := parseDevNum(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOpenOwned(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(file, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	f, size, err := openOwned(file, os.Getuid())
	if err != nil || size != 1024 {
		t.Fatalf("own file: %d, %v", size, err)
	}
	f.Close()
	if _, _, err := openOwned(file, os.Getuid()+1); err == nil {
		t.Error("a file of another uid was accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openOwned(link, os.Getuid()); err == nil {
		t.Error("a symlink was followed")
	}
	if _, _, err := openOwned(dir, os.Getuid()); err == nil {
		t.Error("a directory was accepted")
	}
}

func TestOpenJail(t *testing.T) {
	orig := JailerRoot
	JailerRoot = t.TempDir()
	t.Cleanup(func() { JailerRoot = orig })
	jail := filepath.Join(JailerRoot, "firecracker", "eph-A", "root")
	if err := os.MkdirAll(jail, 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := openJail(jail)
	if err != nil {
		t.Fatalf("real jail: %v", err)
	}
	f.Close()

	// A symlink anywhere on the way is refused, even one that stays under the root.
	if err := os.Symlink(filepath.Join(JailerRoot, "firecracker"), filepath.Join(JailerRoot, "fc")); err != nil {
		t.Fatal(err)
	}
	if _, err := openJail(filepath.Join(JailerRoot, "fc", "eph-A", "root")); err == nil {
		t.Error("a jail path through a symlink was accepted")
	}
	if err := os.Symlink("/etc", filepath.Join(JailerRoot, "firecracker", "eph-B")); err != nil {
		t.Fatal(err)
	}
	if _, err := openJail(filepath.Join(JailerRoot, "firecracker", "eph-B")); err == nil {
		t.Error("a jail symlinked out of the root was accepted")
	}
}

// TestCloneDiskReal builds a real dm-snapshot disk. It needs root, dmsetup and loop devices.
func TestCloneDiskReal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for loop and device-mapper devices")
	}
	if _, err := exec.LookPath(dmsetup); err != nil {
		t.Skip("needs dmsetup")
	}
	for _, dev := range []string{"/dev/loop-control", "/dev/mapper/control"} {
		if _, err := os.Stat(dev); err != nil {
			t.Skipf("needs %s", dev)
		}
	}
	if err := loadDMSnapshot(); err != nil {
		t.Skipf("needs the dm_snapshot kernel module: %v", err)
	}
	root := t.TempDir()
	for v, dir := range map[*string]string{&WarmRoot: "warm", &InstanceRoot: "instances", &JailerRoot: "jailer"} {
		orig := *v
		*v = filepath.Join(root, dir)
		t.Cleanup(func() { *v = orig })
	}
	d := CloneDisk{ID: "eph-TESTDISK", Key: "abc123", Jail: filepath.Join(JailerRoot, "firecracker", "eph-TESTDISK", "root")}
	const size = 8 << 20
	warm := filepath.Join(WarmRoot, d.Key, CloneDiskFile)
	cow := filepath.Join(InstanceRoot, d.ID, CloneCOWFile)
	base := bytes.Repeat([]byte("warm"), size/4)
	for path, data := range map[string][]byte{warm: base, cow: nil} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Truncate(cow, size); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.Jail, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createCloneDisk(d, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deleteCloneDisk(d.ID) })
	node := filepath.Join(d.Jail, CloneDiskFile)
	dev, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := dev.ReadAt(got, 4096); err != nil || string(got) != "warm" {
		t.Errorf("read %q, %v; want the warm disk", got, err)
	}
	if _, err := dev.WriteAt([]byte("CLON"), 0); err != nil {
		t.Fatal(err)
	}
	if err := dev.Sync(); err != nil {
		t.Fatal(err)
	}
	dev.Close()
	if data, _ := os.ReadFile(warm); !bytes.Equal(data, base) {
		t.Error("a write to the clone reached the warm disk")
	}

	if err := deleteCloneDisk(d.ID); err != nil {
		t.Fatal(err)
	}
	if dmExists(DMName(d.ID)) {
		t.Error("dm device still there after delete")
	}
	if err := deleteCloneDisk(d.ID); err != nil {
		t.Errorf("deleting a missing disk: %v", err)
	}
}
