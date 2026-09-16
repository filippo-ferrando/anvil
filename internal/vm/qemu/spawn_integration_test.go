//go:build linux

package qemu

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSpawnRealProcessLifecycle spawns a real qemu-system-x86_64 process
// and drives it over real QMP: dial, query-status, graceful stop.
func TestSpawnRealProcessLifecycle(t *testing.T) {
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skip("qemu-system-x86_64 not installed, skipping")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.qcow2")
	createDisk := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, "64M")
	if out, err := createDisk.CombinedOutput(); err != nil {
		t.Fatalf("creating blank disk: %v: %s", err, out)
	}

	cfg := Config{
		CPUs:      1,
		MemoryMiB: 256,
		DiskPath:  diskPath,
		QMPSocket: filepath.Join(dir, "qmp.sock"),
		KVM:       false, // must work under plain TCG too
		SLIRPHostForwards: []HostForward{
			{HostPort: 12222, GuestPort: 22, Protocol: "tcp"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if proc.Pid() == 0 {
		t.Fatal("expected a non-zero pid after Spawn")
	}
	t.Logf("spawned qemu-system-x86_64 pid %d", proc.Pid())

	if err := proc.AttachQMP(ctx); err != nil {
		logData, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
		t.Fatalf("AttachQMP: %v\nqemu log:\n%s", err, logData)
	}
	t.Log("QMP handshake succeeded")

	status, err := proc.QMP.QueryStatus(ctx)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	t.Logf("query-status: %+v", status)
	if !status.Running && !processAlive(proc.Pid()) {
		t.Error("expected the process to be alive right after a successful QMP handshake")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := proc.Stop(stopCtx, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processAlive(proc.Pid()) {
		t.Error("expected the process to be gone after Stop returned")
	}
	t.Log("Stop escalated and the process is confirmed gone")

	_ = proc.Close()
}

// TestSpawnWithMountRealProcess spawns qemu-system-x86_64 with a 9p mount
// attached and confirms it starts and stays alive.
func TestSpawnWithMountRealProcess(t *testing.T) {
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skip("qemu-system-x86_64 not installed, skipping")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.qcow2")
	createDisk := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, "64M")
	if out, err := createDisk.CombinedOutput(); err != nil {
		t.Fatalf("creating blank disk: %v: %s", err, out)
	}
	shareDir := filepath.Join(dir, "share")
	if err := os.MkdirAll(shareDir, 0o750); err != nil {
		t.Fatalf("creating share dir: %v", err)
	}

	cfg := Config{
		CPUs:      1,
		MemoryMiB: 256,
		DiskPath:  diskPath,
		QMPSocket: filepath.Join(dir, "qmp.sock"),
		Mounts: []Mount{
			{HostPath: shareDir, Tag: "mount0"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := proc.AttachQMP(ctx); err != nil {
		logData, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
		t.Fatalf("AttachQMP: %v\nqemu log:\n%s", err, logData)
	}
	if !processAlive(proc.Pid()) {
		t.Error("expected qemu to still be alive with a 9p mount attached")
	}

	_ = proc.Stop(context.Background(), 0)
	_ = proc.Close()
}
