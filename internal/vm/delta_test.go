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
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/image"
)

type noMirrors struct{}

func (noMirrors) ListMirrors(store.MirrorKind) ([]store.Mirror, error) { return nil, nil }
func (noMirrors) GetCloudInit(string) (store.CloudInitConfig, error) {
	return store.CloudInitConfig{}, nil
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// TestDiskDeltaRoundTrip exports a disk delta on one "host" and adopts it on
// another host's identical copy of the base image.
func TestDiskDeltaRoundTrip(t *testing.T) {
	for _, bin := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
	catalog, err := image.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.List()[0]
	dir := t.TempDir()

	srcVault := image.NewVault(filepath.Join(dir, "src-images"))
	srcBase, _ := srcVault.CachedPath(entry)
	if err := os.MkdirAll(filepath.Dir(srcBase), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "qemu-img", "create", "-f", "qcow2", srcBase, "64M")
	run(t, "qemu-io", "-c", "write -P 0xaa 0 16M", srcBase)
	srcDisk := filepath.Join(dir, "src-disk.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", srcBase, srcDisk)
	run(t, "qemu-io", "-c", "write -P 0xbb 1M 1M", srcDisk)

	src := NewBackend(catalog, srcVault, noMirrors{})
	spec := &instance.Spec{Name: "web", VM: &instance.VMSpec{ImageRef: entry.ID, Arch: entry.Arch, DiskPath: srcDisk}}
	sum, err := src.BaseImageChecksum(spec)
	if err != nil {
		t.Fatalf("BaseImageChecksum: %v", err)
	}
	delta := filepath.Join(dir, "staging", "delta.qcow2")
	if err := src.ExportDiskDelta(context.Background(), spec, delta); err != nil {
		t.Fatalf("ExportDiskDelta: %v", err)
	}
	di, _ := os.Stat(delta)
	bi, _ := os.Stat(srcBase)
	if di.Size() >= bi.Size() {
		t.Errorf("expected the delta (%d bytes) to be smaller than the base (%d bytes)", di.Size(), bi.Size())
	}

	dstVault := image.NewVault(filepath.Join(dir, "dst-images"))
	dstBase, _ := dstVault.CachedPath(entry)
	if err := os.MkdirAll(filepath.Dir(dstBase), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(srcBase, dstBase); err != nil {
		t.Fatal(err)
	}
	dst := NewBackend(catalog, dstVault, noMirrors{})

	bad := &instance.Spec{Name: "web", VM: &instance.VMSpec{
		ImageRef: entry.ID, Arch: entry.Arch, SourceDiskPath: delta, SourceDiskBaseSHA256: strings.Repeat("0", 64),
	}}
	badDir := filepath.Join(dir, "bad")
	_ = os.MkdirAll(badDir, 0o755)
	if err := dst.adoptMigratedDisk(context.Background(), bad, badDir, nil); err == nil {
		t.Fatal("expected a base checksum mismatch to fail")
	}
	if _, err := os.Stat(delta); err != nil {
		t.Fatalf("expected a failed adopt to leave the uploaded disk in place: %v", err)
	}

	migrated := &instance.Spec{Name: "web", VM: &instance.VMSpec{
		ImageRef: entry.ID, Arch: entry.Arch, SourceDiskPath: delta, SourceDiskBaseSHA256: sum,
	}}
	instDir := filepath.Join(dir, "instance")
	_ = os.MkdirAll(instDir, 0o755)
	if err := dst.adoptMigratedDisk(context.Background(), migrated, instDir, nil); err != nil {
		t.Fatalf("adoptMigratedDisk: %v", err)
	}
	if migrated.VM.SourceDiskBaseSHA256 != "" {
		t.Error("expected SourceDiskBaseSHA256 to be cleared once adopted")
	}
	backing, err := image.BackingFile(migrated.VM.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if backing != dstBase {
		t.Errorf("expected the adopted disk to point at %s, got %s", dstBase, backing)
	}
	run(t, "qemu-img", "compare", srcDisk, migrated.VM.DiskPath)
}
