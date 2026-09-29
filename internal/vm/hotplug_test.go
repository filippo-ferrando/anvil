//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

func TestHotplugHeadroom(t *testing.T) {
	if c, m := hotplugHeadroom("aarch64", 2, 1024); c != 0 || m != 0 {
		t.Errorf("expected no headroom on aarch64, got %d cpus / %d MiB", c, m)
	}
	c, m := hotplugHeadroom("x86_64", 2, 1024)
	if c < 2 || c > maxHotplugCPUs || m < 1024 {
		t.Errorf("unexpected headroom %d cpus / %d MiB", c, m)
	}
	if c, _ := hotplugHeadroom("x86_64", 200, 1024); c != 200 {
		t.Errorf("expected a VM asking for more than the cap to keep its own count, got %d", c)
	}
}

// hotplugVM starts a guest-less QEMU with room for 4 vCPUs and 1 GiB more memory.
func hotplugVM(t *testing.T, b *Backend) *instance.Spec {
	t.Helper()
	for _, bin := range []string{"qemu-system-x86_64", "qemu-img"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
	root, err := os.MkdirTemp("", "anvil-hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })

	const id = "HP"
	dir := instanceDir(id)
	_ = os.MkdirAll(dir, 0o750)
	disk := filepath.Join(dir, "disk.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", disk, "64M")
	cfg := qemu.Config{CPUs: 1, MemoryMiB: 256, MaxCPUs: 4, MaxMemoryMiB: 1280, KVM: kvmAvailable(),
		DiskPath: disk, QMPSocket: filepath.Join(dir, "qmp.sock")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc, err := qemu.Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(context.Background(), 0); _ = proc.Close() })
	if err := proc.AttachQMP(ctx); err != nil {
		t.Fatalf("AttachQMP: %v", err)
	}
	if err := saveRuntimeState(dir, runtimeState{Pid: proc.Pid(), BootMemoryMiB: 256, MaxCPUs: 4, MaxMemoryMiB: 1280}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.running[id] = proc
	b.mu.Unlock()
	return &instance.Spec{ID: id, Name: "hp", VM: &instance.VMSpec{CPUs: 1, MemoryMiB: 256}}
}

func TestSetCPUsLiveAddsVCPUs(t *testing.T) {
	b := NewBackend(nil, nil, noMirrors{})
	spec := hotplugVM(t, b)
	if _, err := b.SetCPUsLive(context.Background(), spec, 3); err != nil {
		t.Fatalf("SetCPUsLive: %v", err)
	}
	proc := b.running[spec.ID]
	slots, err := proc.QMP.HotpluggableCPUs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range slots {
		if s.QOMPath != "" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("expected 3 vCPUs plugged, got %d", n)
	}
	if _, err := b.SetCPUsLive(context.Background(), spec, 5); !errors.Is(err, instance.ErrNeedsRestart) {
		t.Errorf("expected more than the boot maximum to need a restart, got %v", err)
	}
}

func TestSetMemoryLiveLimits(t *testing.T) {
	b := NewBackend(nil, nil, noMirrors{})
	spec := hotplugVM(t, b)
	for _, mib := range []int64{128, 2048} {
		if _, err := b.SetMemoryLive(context.Background(), spec, mib); !errors.Is(err, instance.ErrNeedsRestart) {
			t.Errorf("SetMemoryLive(%d): expected a restart to be needed, got %v", mib, err)
		}
	}
}

func TestLiveChangesNeedARunningVM(t *testing.T) {
	b := NewBackend(nil, nil, noMirrors{})
	spec := &instance.Spec{ID: "OFF", Name: "off", VM: &instance.VMSpec{}}
	if _, err := b.SetCPUsLive(context.Background(), spec, 2); !errors.Is(err, instance.ErrNeedsRestart) {
		t.Errorf("expected ErrNeedsRestart for a stopped VM, got %v", err)
	}
}
