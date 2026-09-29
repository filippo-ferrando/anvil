//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

// spawnBlankVM starts a real QEMU with an empty disk, tracked by b as instance id.
func spawnBlankVM(t *testing.T, b *Backend, id string) *qemu.Process {
	t.Helper()
	for _, bin := range []string{"qemu-system-x86_64", "qemu-img"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
	dir, err := os.MkdirTemp("", "anvil-exit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	disk := filepath.Join(dir, "disk.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", disk, "64M")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc, err := qemu.Spawn(ctx, qemu.Config{CPUs: 1, MemoryMiB: 256, DiskPath: disk, QMPSocket: filepath.Join(dir, "qmp.sock")}, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(context.Background(), 0) })
	if err := proc.AttachQMP(ctx); err != nil {
		t.Fatalf("AttachQMP: %v", err)
	}
	b.mu.Lock()
	b.running[id] = proc
	b.mu.Unlock()
	return proc
}

type exitRecord struct {
	id    string
	state instance.State
}

func hookedBackend(t *testing.T) (*Backend, chan exitRecord) {
	t.Helper()
	orig := instanceDir
	tmp := t.TempDir()
	instanceDir = func(id string) string { return filepath.Join(tmp, id) }
	t.Cleanup(func() { instanceDir = orig })

	b := NewBackend(nil, nil, noMirrors{})
	exits := make(chan exitRecord, 4)
	b.SetExitHook(func(id string, state instance.State) { exits <- exitRecord{id, state} })
	return b, exits
}

func waitExitRecord(t *testing.T, exits chan exitRecord) exitRecord {
	t.Helper()
	select {
	case rec := <-exits:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the exit hook")
		return exitRecord{}
	}
}

func TestExitOnItsOwnIsReported(t *testing.T) {
	b, exits := hookedBackend(t)
	proc := spawnBlankVM(t, b, "CLEAN")
	go b.watchExit("CLEAN", "", proc)

	// A guest poweroff ends QEMU the same way a QMP quit does: exit status 0.
	_ = proc.QMP.Quit(context.Background())
	rec := waitExitRecord(t, exits)
	if rec.id != "CLEAN" || rec.state != instance.StateStopped {
		t.Errorf("got %+v, want CLEAN stopped", rec)
	}
	b.mu.Lock()
	_, still := b.running["CLEAN"]
	b.mu.Unlock()
	if still {
		t.Error("expected the exited VM to be dropped from the running set")
	}
}

func TestCrashIsReportedAsError(t *testing.T) {
	b, exits := hookedBackend(t)
	proc := spawnBlankVM(t, b, "CRASH")
	go b.watchExit("CRASH", "", proc)

	_ = syscall.Kill(proc.Pid(), syscall.SIGKILL)
	rec := waitExitRecord(t, exits)
	if rec.id != "CRASH" || rec.state != instance.StateError {
		t.Errorf("got %+v, want CRASH error", rec)
	}
}

func TestStopIsNotReportedAsExit(t *testing.T) {
	b, exits := hookedBackend(t)
	proc := spawnBlankVM(t, b, "STOPPED")
	go b.watchExit("STOPPED", "", proc)

	spec := &instance.Spec{ID: "STOPPED", Name: "stopped", VM: &instance.VMSpec{}}
	if err := b.Stop(context.Background(), spec, true, 0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case rec := <-exits:
		t.Errorf("expected no exit report for a Stop, got %+v", rec)
	case <-time.After(500 * time.Millisecond):
	}
}
