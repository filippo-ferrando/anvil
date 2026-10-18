//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSpawnRealProcessLifecycle boots a real firecracker with the kernel in
// ANVIL_FC_KERNEL and no disk: the kernel starts, finds no root and stays in its panic
// (panic=0), which is enough to drive the API and stop the VM.
func TestSpawnRealProcessLifecycle(t *testing.T) {
	if _, err := exec.LookPath(Binary); err != nil {
		t.Skip("firecracker not installed, skipping")
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skip("/dev/kvm not usable, skipping")
	} else {
		f.Close()
	}
	kernel := os.Getenv("ANVIL_FC_KERNEL")
	if kernel == "" {
		t.Skip("ANVIL_FC_KERNEL not set, skipping")
	}

	dir := t.TempDir()
	paths := Paths{
		APISocket:  filepath.Join(dir, "api.sock"),
		ConfigFile: filepath.Join(dir, "config.json"),
		ConsoleLog: filepath.Join(dir, "console.log"),
		LogFile:    filepath.Join(dir, "firecracker.log"),
	}
	cfg := Config{
		BootSource:    BootSource{KernelImagePath: kernel, BootArgs: "console=ttyS0 reboot=k panic=0"},
		MachineConfig: MachineConfig{VCPUCount: 1, MemSizeMiB: 256},
		Vsock:         &Vsock{GuestCID: GuestCID, UDSPath: filepath.Join(dir, "vsock.sock")},
		Entropy:       &struct{}{},
	}
	if err := cfg.WriteFile(paths.ConfigFile); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc, err := Spawn(ctx, paths)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Every t.Fatal below would otherwise leave a real firecracker running.
	t.Cleanup(func() {
		_ = proc.Stop(context.Background(), 0)
		_ = proc.Close()
	})
	if err := proc.WaitAPI(ctx); err != nil {
		logData, _ := os.ReadFile(paths.LogFile)
		t.Fatalf("WaitAPI: %v\nfirecracker log:\n%s", err, logData)
	}

	info, err := proc.API.Info(ctx)
	if err != nil || info.State != StateRunning {
		t.Fatalf("Info = %+v, %v; want Running", info, err)
	}
	t.Logf("firecracker %s running as pid %d", info.VMMVersion, proc.Pid())

	if err := proc.API.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if info, _ := proc.API.Info(ctx); info.State != StatePaused {
		t.Fatalf("state after Pause = %q", info.State)
	}
	if err := proc.API.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The guest kernel's banner on the serial console proves the boot got that far.
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, _ := os.ReadFile(paths.ConsoleLog)
		if bytes.Contains(data, []byte("Linux version")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no kernel banner on the console after 15s:\n%s", data)
		}
		time.Sleep(200 * time.Millisecond)
	}

	reattached, err := Attach(paths, proc.Pid())
	if err != nil {
		t.Fatalf("Attach to the running process: %v", err)
	}
	if _, err := reattached.API.Info(ctx); err != nil {
		t.Fatalf("Info through the reattached process: %v", err)
	}

	// A panicked guest ignores Ctrl+Alt+Del, so Stop has to fall back to a kill.
	if err := proc.Stop(ctx, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processAlive(proc.Pid()) {
		t.Fatal("process still alive after Stop")
	}
	select {
	case <-reattached.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the reattached process never noticed the exit")
	}
}

func TestAttachRejectsOtherProcess(t *testing.T) {
	if _, err := Attach(Paths{APISocket: "/nonexistent/api.sock"}, os.Getpid()); err == nil {
		t.Fatal("Attach accepted the test binary as a firecracker process")
	}
}
