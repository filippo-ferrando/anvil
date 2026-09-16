package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOverlayForUsesExistingPreparedImage(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	v := NewVault(filepath.Join(dir, "prepared"))

	entry := DistroEntry{
		ID:         "fake-distro",
		Arch:       "x86_64",
		URL:        "https://example.invalid/should-not-be-fetched.qcow2",
		MinDiskGiB: 1,
	}

	// Pre-place a "prepared" base image so this doesn't need to download.
	if err := os.MkdirAll(filepath.Dir(v.preparedPath(entry)), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	createBase := exec.Command("qemu-img", "create", "-f", "qcow2", v.preparedPath(entry), "64M")
	if out, err := createBase.CombinedOutput(); err != nil {
		t.Fatalf("creating fake base image: %v: %s", err, out)
	}

	overlayPath := filepath.Join(dir, "instance", "disk.qcow2")
	if err := v.OverlayFor(context.Background(), entry, overlayPath, 1, nil); err != nil {
		t.Fatalf("OverlayFor: %v", err)
	}

	info, err := os.Stat(overlayPath)
	if err != nil {
		t.Fatalf("expected overlay to exist: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty overlay file")
	}
}

func TestOverlayForRejectsShrinkingBelowBaseImageSize(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	v := NewVault(filepath.Join(dir, "prepared"))
	entry := DistroEntry{ID: "fake", Arch: "x86_64", URL: "https://example.invalid/x.qcow2"}

	if err := os.MkdirAll(filepath.Dir(v.preparedPath(entry)), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Base bigger than the disk size we're about to request.
	createBase := exec.Command("qemu-img", "create", "-f", "qcow2", v.preparedPath(entry), "2G")
	if out, err := createBase.CombinedOutput(); err != nil {
		t.Fatalf("creating fake base image: %v: %s", err, out)
	}

	err := v.OverlayFor(context.Background(), entry, filepath.Join(dir, "instance", "disk.qcow2"), 1, nil)
	if err == nil {
		t.Error("expected an error when the requested disk is smaller than the base image's own virtual size")
	}
}

// TestOverlayForDefaultSizeInheritsBaseImageSize checks that diskGiB=0
// makes the overlay inherit the base image's own virtual size.
func TestOverlayForDefaultSizeInheritsBaseImageSize(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	v := NewVault(filepath.Join(dir, "prepared"))
	entry := DistroEntry{ID: "fake", Arch: "x86_64", URL: "https://example.invalid/x.qcow2", MinDiskGiB: 3}

	if err := os.MkdirAll(filepath.Dir(v.preparedPath(entry)), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Bigger than entry.MinDiskGiB on purpose, matching the real scenario.
	createBase := exec.Command("qemu-img", "create", "-f", "qcow2", v.preparedPath(entry), "5G")
	if out, err := createBase.CombinedOutput(); err != nil {
		t.Fatalf("creating fake base image: %v: %s", err, out)
	}

	overlayPath := filepath.Join(dir, "instance", "disk.qcow2")
	if err := v.OverlayFor(context.Background(), entry, overlayPath, 0, nil); err != nil {
		t.Fatalf("OverlayFor with default (0) disk size: %v", err)
	}

	size, err := qemuImgVirtualSize(overlayPath)
	if err != nil {
		t.Fatalf("qemuImgVirtualSize: %v", err)
	}
	if want := int64(5) * bytesPerGiB; size != want {
		t.Errorf("expected the overlay to inherit the base image's 5GiB virtual size, got %d bytes (want %d)", size, want)
	}
}

func TestVaultList(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	v := NewVault(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entry := DistroEntry{ID: "ubuntu-24.04", Arch: "x86_64", URL: "https://example.invalid/x.qcow2"}
	create := exec.Command("qemu-img", "create", "-f", "qcow2", v.preparedPath(entry), "16M")
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("creating fake base image: %v: %s", err, out)
	}
	// A stray non-qcow2 file in the same dir shouldn't show up in List.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o640); err != nil {
		t.Fatalf("writing stray file: %v", err)
	}

	images, err := v.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected exactly one cached image, got %d: %+v", len(images), images)
	}
	if images[0].ID != "ubuntu-24.04" || images[0].Arch != "x86_64" {
		t.Errorf("expected id=ubuntu-24.04 arch=x86_64, got id=%s arch=%s", images[0].ID, images[0].Arch)
	}
	if images[0].SizeBytes == 0 {
		t.Error("expected a non-zero size")
	}
}

func TestVaultDelete(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	v := NewVault(dir)
	entry := DistroEntry{ID: "fake", Arch: "x86_64", URL: "https://example.invalid/x.qcow2"}
	path := v.preparedPath(entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	create := exec.Command("qemu-img", "create", "-f", "qcow2", path, "16M")
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("creating fake base image: %v: %s", err, out)
	}

	if err := v.Delete(path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected the file to be gone, stat err: %v", err)
	}

	// Deleting an already-gone file is not an error.
	if err := v.Delete(path); err != nil {
		t.Errorf("Delete on an already-removed file should be a no-op, got: %v", err)
	}
}

func TestListSnapshots(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "disk.qcow2")
	create := exec.Command("qemu-img", "create", "-f", "qcow2", path, "16M")
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("creating disk: %v: %s", err, out)
	}

	if snaps, err := ListSnapshots(path); err != nil {
		t.Fatalf("ListSnapshots on a fresh disk: %v", err)
	} else if len(snaps) != 0 {
		t.Fatalf("expected no snapshots yet, got %+v", snaps)
	}

	for _, name := range []string{"before-upgrade", "after-upgrade"} {
		cmd := exec.Command("qemu-img", "snapshot", "-c", name, path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("creating snapshot %q: %v: %s", name, err, out)
		}
	}

	snaps, err := ListSnapshots(path)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d: %+v", len(snaps), snaps)
	}
	if snaps[0].Name != "before-upgrade" || snaps[1].Name != "after-upgrade" {
		t.Errorf("expected oldest-first order [before-upgrade after-upgrade], got [%s %s]", snaps[0].Name, snaps[1].Name)
	}
	for _, s := range snaps {
		if s.HasVMState {
			t.Errorf("snapshot %q created via qemu-img -c should be disk-only, not HasVMState", s.Name)
		}
		if s.CreatedAt.IsZero() {
			t.Errorf("snapshot %q has a zero CreatedAt", s.Name)
		}
	}

	del := exec.Command("qemu-img", "snapshot", "-d", "before-upgrade", path)
	if out, err := del.CombinedOutput(); err != nil {
		t.Fatalf("deleting snapshot: %v: %s", err, out)
	}
	if snaps, err := ListSnapshots(path); err != nil {
		t.Fatalf("ListSnapshots after delete: %v", err)
	} else if len(snaps) != 1 || snaps[0].Name != "after-upgrade" {
		t.Fatalf("expected only after-upgrade left, got %+v", snaps)
	}
}

func TestBackingFile(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	v := NewVault(filepath.Join(dir, "prepared"))
	entry := DistroEntry{ID: "fake", Arch: "x86_64", URL: "https://example.invalid/x.qcow2"}

	if err := os.MkdirAll(filepath.Dir(v.preparedPath(entry)), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	createBase := exec.Command("qemu-img", "create", "-f", "qcow2", v.preparedPath(entry), "16M")
	if out, err := createBase.CombinedOutput(); err != nil {
		t.Fatalf("creating fake base image: %v: %s", err, out)
	}

	overlayPath := filepath.Join(dir, "instance", "disk.qcow2")
	if err := v.OverlayFor(context.Background(), entry, overlayPath, 0, nil); err != nil {
		t.Fatalf("OverlayFor: %v", err)
	}

	backing, err := BackingFile(overlayPath)
	if err != nil {
		t.Fatalf("BackingFile: %v", err)
	}
	if backing != v.preparedPath(entry) {
		t.Errorf("expected backing file %q, got %q", v.preparedPath(entry), backing)
	}
}
