//go:build linux

package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// adopted creates an instance from a transferred qcow2, the way a migration or import lands.
func adopted(t *testing.T, b *FCBackend, id, disk, baseSHA string) (*instance.Spec, error) {
	t.Helper()
	spec := &instance.Spec{ID: id, Name: id, Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "fc-test", Hypervisor: instance.VMHypervisorFirecracker,
		SourceDiskPath: disk, SourceDiskBaseSHA256: baseSHA, DefaultUser: "kept",
		// On an intent bridge, so the address question stays out of this.
		NetworkMode: "bridge",
	}}
	return spec, b.Create(context.Background(), spec, nil)
}

func TestFCTransferRoundTrip(t *testing.T) {
	b := fcTestBackend(t)
	ctx := context.Background()
	src := createdFCVM(t, b, "SRC")
	writeAt(t, src.VM.DiskPath, "migrated data")
	staging := t.TempDir()

	sum, err := b.BaseImageChecksum(src)
	if err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(staging, "delta.qcow2")
	if err := b.ExportDiskDelta(ctx, src, delta); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(staging, "full.qcow2")
	if err := b.ExportDisk(ctx, src, full); err != nil {
		t.Fatal(err)
	}
	if backing, _ := image.BackingFile(delta); backing == "" {
		t.Fatal("the delta has no backing file")
	}

	for _, c := range []struct{ id, disk, sha string }{{"FROMDELTA", delta, sum}, {"FROMFULL", full, ""}} {
		dest, err := adopted(t, b, c.id, c.disk, c.sha)
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if got := readAt(t, dest.VM.DiskPath, 13); got != "migrated data" {
			t.Errorf("%s: adopted disk holds %q", c.id, got)
		}
		srcInfo, _ := os.Stat(src.VM.DiskPath)
		if info, _ := os.Stat(dest.VM.DiskPath); info.Size() != srcInfo.Size() || filepath.Ext(dest.VM.DiskPath) != ".raw" {
			t.Errorf("%s: adopted disk %s is %d bytes, want a raw %d", c.id, dest.VM.DiskPath, info.Size(), srcInfo.Size())
		}
		if dest.VM.SourceDiskPath != "" || dest.VM.DefaultUser != "kept" || dest.VM.SeedISOPath != "" {
			t.Errorf("%s: spec after adopt = %+v", c.id, dest.VM)
		}
		if _, err := os.Stat(c.disk); !os.IsNotExist(err) {
			t.Errorf("%s: the transferred file is still there", c.id)
		}
	}

	// A bundle: the file it carries, then the import side.
	bundled, err := b.ExportDiskFile(ctx, src, staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareImportedDisk(ctx, "fc-test", "x86_64", bundled, sum); err != nil {
		t.Fatal(err)
	}
	if err := b.PrepareImportedDisk(ctx, "fc-test", "x86_64", bundled, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a bundle made against another base image was accepted")
	}
	imported, err := adopted(t, b, "IMPORTED", bundled, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, imported.VM.DiskPath, 13); got != "migrated data" {
		t.Fatalf("imported disk holds %q", got)
	}
}

func TestFCAdoptKeepsAddress(t *testing.T) {
	b := fcTestBackend(t)
	ctx := context.Background()
	src := createdFCVM(t, b, "SRC")
	disk := filepath.Join(t.TempDir(), "d.qcow2")
	if err := b.ExportDisk(ctx, src, disk); err != nil {
		t.Fatal(err)
	}

	clash := &instance.Spec{ID: "CLASH", Name: "clash", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "fc-test", Hypervisor: instance.VMHypervisorFirecracker,
		SourceDiskPath: disk, StaticIP: src.VM.StaticIP,
	}}
	// A copy next to its original gets a new address, told to the guest through a new seed.
	if err := b.Create(ctx, clash, nil); err != nil {
		t.Fatal(err)
	}
	if clash.VM.StaticIP == "" || clash.VM.StaticIP == src.VM.StaticIP || clash.VM.SeedISOPath == "" {
		t.Fatalf("adopting with an address in use: ip %q (original %q), seed %q", clash.VM.StaticIP, src.VM.StaticIP, clash.VM.SeedISOPath)
	}

	if err := b.ExportDisk(ctx, src, disk); err != nil {
		t.Fatal(err)
	}
	moved := &instance.Spec{ID: "MOVED", Name: "moved", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "fc-test", Hypervisor: instance.VMHypervisorFirecracker,
		SourceDiskPath: disk, StaticIP: "10.254.254.77/24",
	}}
	if err := b.Create(ctx, moved, nil); err != nil {
		t.Fatal(err)
	}
	if moved.VM.StaticIP != "10.254.254.77/24" || moved.VM.Gateway != "10.254.254.1" {
		t.Fatalf("adopted address = %q via %q, want the one it came with", moved.VM.StaticIP, moved.VM.Gateway)
	}
}
