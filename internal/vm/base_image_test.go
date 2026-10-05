//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// testManifest describes one fake distro with no published checksum, so the
// vault accepts the file these tests put in place instead of downloading one.
const testManifest = `{"schema_version":1,"distros":[{"id":"test-distro","name":"Test",
"distro":"test","version":"1","arch":"x86_64","url":"https://example.invalid/x.qcow2",
"sha256":"","min_disk_gib":1,"default_user":"test"}]}`

// testCatalog builds a backend over a fake one-distro catalog and an empty
// image cache under dir, plus that distro's entry.
func testCatalog(t *testing.T, dir string) (*Backend, *image.Vault, image.DistroEntry) {
	t.Helper()
	catalog, err := (&image.Catalog{}).WithMirrors([]image.MirrorManifest{{ManifestJSON: testManifest, Priority: 1}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := catalog.Find("test-distro", "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	vault := image.NewVault(filepath.Join(dir, "images"))
	return NewBackend(catalog, vault, noMirrors{}), vault, entry
}

// newBaseAndDisk creates a base image and an overlay disk on top of it.
func newBaseAndDisk(t *testing.T, basePath, diskPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "qemu-img", "create", "-f", "qcow2", basePath, "64M")
	run(t, "qemu-io", "-c", "write -P 0xaa 0 16M", basePath)
	run(t, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", basePath, diskPath)
	run(t, "qemu-io", "-c", "write -P 0xbb 1M 1M", diskPath)
}

func requireQemuImg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
}

// TestAdoptMigratedDiskTakesSentBase covers a migration to a host that has no
// copy of the base image, so the base travels with the delta.
func TestAdoptMigratedDiskTakesSentBase(t *testing.T) {
	requireQemuImg(t)
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	srcBase := filepath.Join(srcDir, "base.qcow2")
	srcDisk := filepath.Join(srcDir, "disk.qcow2")
	newBaseAndDisk(t, srcBase, srcDisk)

	src, _, entry := testCatalog(t, filepath.Join(dir, "src-host"))
	spec := &instance.Spec{Name: "web", VM: &instance.VMSpec{ImageRef: entry.ID, Arch: entry.Arch, DiskPath: srcDisk}}
	if path, err := src.BaseImagePath(spec); err != nil || path != srcBase {
		t.Fatalf("BaseImagePath = %q, %v; want %q", path, err, srcBase)
	}
	sum, err := src.BaseImageChecksum(spec)
	if err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(dir, "staging", "delta.qcow2")
	if err := src.ExportDiskDelta(context.Background(), spec, delta); err != nil {
		t.Fatal(err)
	}

	// Both files land in the target's staging dir, as the upload leaves them.
	sentBase := filepath.Join(dir, "staging", "base.qcow2")
	if err := copyFile(srcBase, sentBase); err != nil {
		t.Fatal(err)
	}

	dst, dstVault, _ := testCatalog(t, filepath.Join(dir, "dst-host"))
	if _, cached := dstVault.CachedPath(entry); cached {
		t.Fatal("the destination host should start with no cached base image")
	}

	migrated := &instance.Spec{Name: "web", VM: &instance.VMSpec{
		ImageRef: entry.ID, Arch: entry.Arch,
		SourceDiskPath: delta, SourceDiskBaseSHA256: sum, SourceBaseImagePath: sentBase,
	}}
	instDir := filepath.Join(dir, "instance")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dst.adoptMigratedDisk(context.Background(), migrated, instDir, nil); err != nil {
		t.Fatalf("adoptMigratedDisk: %v", err)
	}

	cachedBase, cached := dstVault.CachedPath(entry)
	if !cached {
		t.Fatal("expected the sent base image to be cached for the next migration")
	}
	backing, err := image.BackingFile(migrated.VM.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if backing != cachedBase {
		t.Errorf("adopted disk points at %s, want the cached base %s", backing, cachedBase)
	}
	if migrated.VM.SourceBaseImagePath != "" || migrated.VM.SourceDiskBaseSHA256 != "" {
		t.Error("expected the migration fields to be cleared once adopted")
	}
	run(t, "qemu-img", "compare", srcDisk, migrated.VM.DiskPath)
}

// TestAdoptMigratedDiskRejectsCorruptSentBase makes sure a base image that
// arrives damaged is never adopted.
func TestAdoptMigratedDiskRejectsCorruptSentBase(t *testing.T) {
	requireQemuImg(t)
	dir := t.TempDir()

	srcBase := filepath.Join(dir, "src", "base.qcow2")
	srcDisk := filepath.Join(dir, "src", "disk.qcow2")
	newBaseAndDisk(t, srcBase, srcDisk)

	src, _, entry := testCatalog(t, filepath.Join(dir, "src-host"))
	spec := &instance.Spec{Name: "web", VM: &instance.VMSpec{ImageRef: entry.ID, Arch: entry.Arch, DiskPath: srcDisk}}
	sum, err := src.BaseImageChecksum(spec)
	if err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(dir, "staging", "delta.qcow2")
	if err := src.ExportDiskDelta(context.Background(), spec, delta); err != nil {
		t.Fatal(err)
	}

	// A base image that isn't the one the delta was made against.
	sentBase := filepath.Join(dir, "staging", "base.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", sentBase, "64M")

	dst, dstVault, _ := testCatalog(t, filepath.Join(dir, "dst-host"))
	migrated := &instance.Spec{Name: "web", VM: &instance.VMSpec{
		ImageRef: entry.ID, Arch: entry.Arch,
		SourceDiskPath: delta, SourceDiskBaseSHA256: sum, SourceBaseImagePath: sentBase,
	}}
	instDir := filepath.Join(dir, "instance")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err = dst.adoptMigratedDisk(context.Background(), migrated, instDir, nil)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected a corrupt base image to be rejected, got %v", err)
	}
	if _, cached := dstVault.CachedPath(entry); cached {
		t.Error("a rejected base image must not end up in the image cache")
	}
	if _, err := os.Stat(delta); err != nil {
		t.Errorf("expected a failed adopt to leave the uploaded disk in place: %v", err)
	}
}

// TestPrepareImportedDiskChecksBase covers `anvil import` refusing a bundle
// whose base image differs from this host's copy.
func TestPrepareImportedDiskChecksBase(t *testing.T) {
	requireQemuImg(t)
	dir := t.TempDir()

	b, vault, entry := testCatalog(t, dir)
	base, _ := vault.CachedPath(entry)
	disk := filepath.Join(dir, "imported.qcow2")
	newBaseAndDisk(t, base, disk)

	err := b.PrepareImportedDisk(context.Background(), entry.ID, entry.Arch, disk, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("expected a different base image to be rejected, got %v", err)
	}

	sum, err := image.FileChecksum(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareImportedDisk(context.Background(), entry.ID, entry.Arch, disk, sum); err != nil {
		t.Fatalf("PrepareImportedDisk with the matching base: %v", err)
	}
	backing, err := image.BackingFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if backing != base {
		t.Errorf("imported disk points at %s, want %s", backing, base)
	}
}
