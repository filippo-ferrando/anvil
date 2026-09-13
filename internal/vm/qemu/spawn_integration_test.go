package qemu

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSpawnRealProcessLifecycle actually spawns qemu-system-x86_64 (no
// mocking) and drives it over real QMP: dial, query-status, graceful stop
// (which — since there's no real guest OS on the blank disk to answer an
// ACPI power button press — is expected to time out and escalate to QMP
// quit, then SIGKILL if even that doesn't land in time). This is the
// closest thing to an integration test this package can run without a real
// cloud image (network) or KVM: it validates the actual process/QMP
// control-plane machinery end to end, not just BuildArgs' string output.
//
// What this does NOT prove: that a real guest OS actually boots and
// cloud-init actually runs inside it — that needs a real base image
// (network) and is a materially different, still-open verification step.
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
		KVM:       false, // no /dev/kvm assumption — this must work under plain TCG too
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

	// No real guest OS is present to answer an ACPI shutdown, so give the
	// graceful phase a short timeout and confirm Stop still gets there via
	// escalation (QMP quit, or SIGKILL) rather than hanging.
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

// TestSpawnWithMountRealProcess actually spawns qemu-system-x86_64 with a 9p
// mount attached and confirms it starts and stays alive — this is what was
// empirically checked by hand (real `qemu-system-x86_64` version 11.1.1,
// `qom-list-types` showing no user-creatable fsdev-backend object at all,
// confirming there's no QMP hotplug path for a 9p share) before committing
// to the 9p-at-launch-only design in the first place; committed here so
// that finding stays verified instead of just remembered.
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
