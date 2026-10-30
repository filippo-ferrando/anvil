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

	vmproc "git.fferrando.cc/rdfilippo/anvil/internal/vm/proc"
)

// TestSpawnRealProcessLifecycle boots a real firecracker with the kernel in
// ANVIL_FC_KERNEL and no disk: the kernel starts, finds no root and stays in its panic
// (panic=0), which is enough to drive the API and stop the VM.
func TestSpawnRealProcessLifecycle(t *testing.T) {
	kernel := realKernel(t)

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
	waitConsole(t, paths.ConsoleLog, "Linux version")

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
	if vmproc.Alive(proc.Pid()) {
		t.Fatal("process still alive after Stop")
	}
	select {
	case <-reattached.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the reattached process never noticed the exit")
	}
}

// realKernel skips t unless firecracker, /dev/kvm and ANVIL_FC_KERNEL are all usable.
func realKernel(t *testing.T) string {
	t.Helper()
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
	return kernel
}

// TestSnapshotRestore snapshots a VM with a warm VM's devices (vsock, entropy, serial, two
// read-only drives) and loads it in a new firecracker started without a config file.
func TestSnapshotRestore(t *testing.T) {
	kernel := realKernel(t)
	dir := t.TempDir()
	paths := Paths{
		APISocket:  filepath.Join(dir, "api.sock"),
		ConfigFile: filepath.Join(dir, "config.json"),
		ConsoleLog: filepath.Join(dir, "console.log"),
		LogFile:    filepath.Join(dir, "firecracker.log"),
	}
	vsock := filepath.Join(dir, "vsock.sock")
	var drives []Drive
	for _, id := range []string{"seed", "guest"} {
		f := filepath.Join(dir, id+".iso")
		if err := os.WriteFile(f, make([]byte, 1<<20), 0o640); err != nil {
			t.Fatal(err)
		}
		drives = append(drives, Drive{DriveID: id, PathOnHost: f, IsReadOnly: true})
	}
	const memMiB = 256
	cfg := Config{
		BootSource:    BootSource{KernelImagePath: kernel, BootArgs: "console=ttyS0 reboot=k panic=0"},
		MachineConfig: MachineConfig{VCPUCount: 1, MemSizeMiB: memMiB},
		Drives:        drives,
		Vsock:         &Vsock{GuestCID: GuestCID, UDSPath: vsock},
		Entropy:       &struct{}{},
	}
	if err := cfg.WriteFile(paths.ConfigFile); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	spawn := func(p Paths) *Process {
		t.Helper()
		proc, err := Spawn(ctx, p)
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		t.Cleanup(func() {
			_ = proc.Stop(context.Background(), 0)
			_ = proc.Close()
		})
		if err := proc.WaitAPI(ctx); err != nil {
			logData, _ := os.ReadFile(p.LogFile)
			t.Fatalf("WaitAPI: %v\nfirecracker log:\n%s", err, logData)
		}
		return proc
	}

	src := spawn(paths)
	waitConsole(t, paths.ConsoleLog, "Linux version")
	if err := src.API.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	state, mem := filepath.Join(dir, "vmstate"), filepath.Join(dir, "mem")
	start := time.Now()
	if err := src.API.CreateSnapshot(ctx, SnapshotCreate{SnapshotPath: state, MemFilePath: mem, SnapshotType: SnapshotFull}); err != nil {
		logData, _ := os.ReadFile(paths.LogFile)
		t.Fatalf("CreateSnapshot: %v\nfirecracker log:\n%s", err, logData)
	}
	t.Logf("snapshot of %d MiB took %s", memMiB, time.Since(start).Round(time.Millisecond))
	if st, err := os.Stat(mem); err != nil || st.Size() != memMiB<<20 {
		t.Fatalf("memory file = %v, %v; want %d MiB", st, err, memMiB)
	}
	if err := src.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}

	// The restored VM binds the same vsock socket, so the old one has to go.
	_ = os.Remove(vsock)
	restore := paths
	restore.ConfigFile = ""
	dst := spawn(restore)
	if info, err := dst.API.Info(ctx); err != nil || info.State != StateNotStarted {
		t.Fatalf("Info before load = %+v, %v; want %q", info, err, StateNotStarted)
	}
	start = time.Now()
	err := dst.API.LoadSnapshot(ctx, SnapshotLoad{
		SnapshotPath: state, MemBackend: MemoryBackend{BackendType: "File", BackendPath: mem}, ResumeVM: true,
	})
	if err != nil {
		logData, _ := os.ReadFile(paths.LogFile)
		t.Fatalf("LoadSnapshot: %v\nfirecracker log:\n%s", err, logData)
	}
	t.Logf("restore took %s", time.Since(start).Round(time.Millisecond))
	if info, err := dst.API.Info(ctx); err != nil || info.State != StateRunning {
		t.Fatalf("Info after load = %+v, %v; want running", info, err)
	}
	if _, err := os.Stat(vsock); err != nil {
		t.Errorf("restored VM has no vsock socket: %v", err)
	}
	select {
	case <-dst.Exited():
		t.Fatalf("restored firecracker exited: %s", logTail(paths.LogFile))
	case <-time.After(time.Second):
	}
}

// waitConsole waits up to 15s for want on the serial console log.
func waitConsole(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		if bytes.Contains(data, []byte(want)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q on the console after 15s:\n%s", want, data)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestAttachRejectsOtherProcess(t *testing.T) {
	if _, err := Attach(Paths{APISocket: "/nonexistent/api.sock"}, os.Getpid()); err == nil {
		t.Fatal("Attach accepted the test binary as a firecracker process")
	}
}
