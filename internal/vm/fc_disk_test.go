//go:build linux

package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

func createdFCVM(t *testing.T, b *FCBackend, id string) *instance.Spec {
	t.Helper()
	spec := &instance.Spec{ID: id, Name: id, Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "fc-test", DiskGiB: 1, Hypervisor: instance.VMHypervisorFirecracker,
	}}
	if err := b.Create(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}
	return spec
}

func writeAt(t *testing.T, path string, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte(data), 1<<20); err != nil {
		t.Fatal(err)
	}
}

func readAt(t *testing.T, path string, n int) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimRight(buf, "\x00"))
}

func TestFCSnapshotsStopped(t *testing.T) {
	b := fcTestBackend(t)
	ctx := context.Background()
	spec := createdFCVM(t, b, "SNAP")
	disk := spec.VM.DiskPath

	writeAt(t, disk, "before")
	if err := b.CreateSnapshot(ctx, spec, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateSnapshot(ctx, spec, "s1"); err == nil {
		t.Error("a second snapshot with the same name should be refused")
	}
	if err := b.CreateSnapshot(ctx, spec, "bad/name"); err == nil {
		t.Error("a name with a slash should be refused")
	}
	snaps, err := b.ListSnapshots(ctx, spec)
	if err != nil || len(snaps) != 1 || snaps[0].Name != "s1" || snaps[0].HasVMState {
		t.Fatalf("ListSnapshots = %+v, %v", snaps, err)
	}

	writeAt(t, disk, "after!")
	if err := b.RestoreSnapshot(ctx, spec, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, disk, 6); got != "before" {
		t.Fatalf("disk after restore holds %q, want the snapshot's content", got)
	}
	if err := b.RestoreSnapshot(ctx, spec, "missing"); err == nil {
		t.Error("restoring a missing snapshot should fail")
	}
	if err := b.DeleteSnapshot(ctx, spec, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteSnapshot(ctx, spec, "s1"); err == nil {
		t.Error("deleting a missing snapshot should fail")
	}
	if snaps, _ := b.ListSnapshots(ctx, spec); len(snaps) != 0 {
		t.Fatalf("snapshots left after delete: %+v", snaps)
	}
}

func TestFCForkStopped(t *testing.T) {
	b := fcTestBackend(t)
	ctx := context.Background()
	source := createdFCVM(t, b, "SRC")
	writeAt(t, source.VM.DiskPath, "source data")

	// What the Manager's fork clone leaves: the source's settings, no disk, address or seed.
	clone := *source.VM
	clone.DiskPath, clone.SeedISOPath, clone.StaticIP, clone.MACAddress = "", "", "", ""
	dest := &instance.Spec{ID: "DST", Name: "dst", Kind: instance.KindVM, VM: &clone}
	if err := b.Fork(ctx, source, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, dest.VM.DiskPath, 11); got != "source data" {
		t.Fatalf("fork disk holds %q", got)
	}
	if dest.VM.DiskPath == source.VM.DiskPath || dest.VM.SeedISOPath == "" {
		t.Fatalf("fork paths: disk %q seed %q", dest.VM.DiskPath, dest.VM.SeedISOPath)
	}
	if dest.VM.StaticIP == "" || dest.VM.StaticIP == source.VM.StaticIP {
		t.Fatalf("fork address %q, source %q: want its own", dest.VM.StaticIP, source.VM.StaticIP)
	}
}

func TestFCResizeAndLiveLimits(t *testing.T) {
	b := fcTestBackend(t)
	ctx := context.Background()
	spec := createdFCVM(t, b, "RSZ")

	if _, err := b.ResizeDisk(ctx, spec, 1); err == nil {
		t.Error("resizing to the same size should be refused")
	}
	note, err := b.ResizeDisk(ctx, spec, 3)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(spec.VM.DiskPath); info.Size() != 3<<30 {
		t.Fatalf("disk is %d bytes after growing to 3GiB (%s)", info.Size(), note)
	}

	if _, err := b.SetCPUsLive(ctx, spec, 4); !errors.Is(err, instance.ErrNeedsRestart) {
		t.Errorf("SetCPUsLive err = %v, want ErrNeedsRestart", err)
	}
	if _, err := b.SetMemoryLive(ctx, spec, 2048); !errors.Is(err, instance.ErrNeedsRestart) {
		t.Errorf("SetMemoryLive on a stopped VM err = %v, want ErrNeedsRestart", err)
	}

	if got := fcHotplugMiB("x86_64", 1, 1024); got%fcMemSlotMiB != 0 || got < hostMemoryMiB()-1024 {
		t.Errorf("fcHotplugMiB = %d, want whole %d MiB slots up to the host's memory", got, fcMemSlotMiB)
	}
	if got := fcHotplugMiB("aarch64", 1, 1024); got != 0 {
		t.Errorf("fcHotplugMiB on aarch64 = %d, want none", got)
	}
}
