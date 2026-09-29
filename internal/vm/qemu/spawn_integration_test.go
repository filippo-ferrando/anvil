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

// TestSpawnWithMountRealProcess spawns qemu-system-x86_64 with a boot-time virtiofs
// mount, hot-plugs a second one over QMP, and confirms both devices are attached.
func TestSpawnWithMountRealProcess(t *testing.T) {
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skip("qemu-system-x86_64 not installed, skipping")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}
	if _, err := VirtiofsdPath(); err != nil {
		t.Skip("virtiofsd not installed, skipping")
	}

	// Unix socket paths are limited to 108 bytes, too short for t.TempDir under long test names.
	dir, err := os.MkdirTemp("", "anvil-fs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	diskPath := filepath.Join(dir, "disk.qcow2")
	createDisk := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, "64M")
	if out, err := createDisk.CombinedOutput(); err != nil {
		t.Fatalf("creating blank disk: %v: %s", err, out)
	}
	var daemons []*Virtiofsd
	t.Cleanup(func() {
		for _, d := range daemons {
			d.Kill()
		}
	})
	share := func(tag string) string {
		shareDir := filepath.Join(dir, tag)
		if err := os.MkdirAll(shareDir, 0o750); err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(dir, tag+".sock")
		d, err := StartVirtiofsd(shareDir, sock, false, filepath.Join(dir, tag+".log"))
		if err != nil {
			t.Fatalf("StartVirtiofsd: %v", err)
		}
		daemons = append(daemons, d)
		return sock
	}

	cfg := Config{
		CPUs:      1,
		MemoryMiB: 256,
		DiskPath:  diskPath,
		QMPSocket: filepath.Join(dir, "qmp.sock"),
		Mounts:    []Mount{{Tag: "mount0", SocketPath: share("mount0")}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		_ = proc.Stop(context.Background(), 0)
		_ = proc.Close()
	}()
	if err := proc.AttachQMP(ctx); err != nil {
		logData, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
		t.Fatalf("AttachQMP: %v\nqemu log:\n%s", err, logData)
	}

	if err := proc.QMP.AddVirtiofs(ctx, "mount1", share("mount1")); err != nil {
		logData, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
		t.Fatalf("AddVirtiofs: %v\nqemu log:\n%s", err, logData)
	}
	for _, tag := range []string{"mount0", "mount1"} {
		present, err := proc.QMP.hasPeripheral(ctx, VirtiofsDeviceID(tag))
		if err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Errorf("expected device for %s to be attached", tag)
		}
	}
	if !processAlive(proc.Pid()) {
		t.Error("expected qemu to still be alive with virtiofs mounts attached")
	}
}
