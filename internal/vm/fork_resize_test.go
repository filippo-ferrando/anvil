//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/image"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

// runningDiskVM spawns a real QEMU on an overlay of a small base image, like an anvil VM's disk.
func runningDiskVM(t *testing.T, b *Backend, id string) (*instance.Spec, *qemu.Process, string) {
	t.Helper()
	for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
	dir, err := os.MkdirTemp("", "anvil-fork")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	base := filepath.Join(dir, "base.qcow2")
	disk := filepath.Join(dir, "disk.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", base, "64M")
	run(t, "qemu-io", "-c", "write -P 0xaa 0 8M", base)
	run(t, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, disk)
	run(t, "qemu-io", "-c", "write -P 0xbb 1M 1M", disk)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := qemu.Config{CPUs: 1, MemoryMiB: 256, DiskPath: disk, QMPSocket: filepath.Join(dir, "qmp.sock"), Disk: qemu.ProbeDiskTuning(ctx, disk)}
	proc, err := qemu.Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(context.Background(), 0); _ = proc.Close() })
	if err := proc.AttachQMP(ctx); err != nil {
		t.Fatalf("AttachQMP: %v", err)
	}
	b.mu.Lock()
	b.running[id] = proc
	b.mu.Unlock()
	return &instance.Spec{ID: id, Name: id, VM: &instance.VMSpec{DiskPath: disk}}, proc, base
}

func TestForkLiveCopiesPointInTime(t *testing.T) {
	b := NewBackend(nil, nil, noMirrors{})
	spec, proc, base := runningDiskVM(t, b, "FORKSRC")
	dest := filepath.Join(filepath.Dir(spec.VM.DiskPath), "fork.qcow2")

	var statuses []string
	if err := b.forkLive(context.Background(), spec, proc, base, dest, func(s string) { statuses = append(statuses, s) }); err != nil {
		t.Fatalf("forkLive: %v", err)
	}
	// The source is still in use by QEMU, so compare with -U.
	run(t, "qemu-img", "compare", "-U", spec.VM.DiskPath, dest)
	backing, err := image.BackingFile(dest)
	if err != nil || backing != base {
		t.Errorf("expected the fork to keep the base image %s, got %q (%v)", base, backing, err)
	}
	t.Logf("progress: %v", statuses)
	if status, err := proc.QMP.QueryStatus(context.Background()); err != nil || !status.Running {
		t.Errorf("expected the source VM still running, got %+v, %v", status, err)
	}
}

func TestResizeDiskLiveAndOffline(t *testing.T) {
	b := NewBackend(nil, nil, noMirrors{})
	spec, proc, _ := runningDiskVM(t, b, "RESIZE")

	if _, err := b.ResizeDisk(context.Background(), spec, 0); err == nil {
		t.Error("expected shrinking to be refused")
	}
	note, err := b.ResizeDisk(context.Background(), spec, 1)
	if err != nil {
		t.Fatalf("live ResizeDisk: %v", err)
	}
	t.Log(note)
	if _, size, _ := image.DiskUsage(spec.VM.DiskPath); size != 1<<30 {
		t.Errorf("expected 1 GiB after the live resize, got %d", size)
	}

	_ = proc.Stop(context.Background(), 0)
	b.mu.Lock()
	delete(b.running, spec.ID)
	b.mu.Unlock()
	if _, err := b.ResizeDisk(context.Background(), spec, 2); err != nil {
		t.Fatalf("offline ResizeDisk: %v", err)
	}
	if _, size, _ := image.DiskUsage(spec.VM.DiskPath); size != 2<<30 {
		t.Errorf("expected 2 GiB after the offline resize, got %d", size)
	}
}
