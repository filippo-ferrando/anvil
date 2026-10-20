//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

// recordingNetworker records Detach calls and attaches nothing.
type recordingNetworker struct{ detached []string }

func (n *recordingNetworker) Attach(id, bridge string) (string, error) { return "tap-" + id, nil }
func (n *recordingNetworker) Detach(id string) error {
	n.detached = append(n.detached, id)
	return nil
}
func (n *recordingNetworker) Stats(string) (uint64, uint64, error) { return 0, 0, nil }

// leftoverBackend is an FCBackend with no firecracker, and an instance dir and jail
// (named after a versioned binary) left behind for id.
func leftoverBackend(t *testing.T, id string) (*FCBackend, *recordingNetworker, string) {
	t.Helper()
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, "instances", id) }
	t.Cleanup(func() { instanceDir = orig })
	net := &recordingNetworker{}
	b := &FCBackend{
		Networker: net, ChrootBase: filepath.Join(root, "jailer"),
		running: map[string]*firecracker.Process{}, starting: map[string]struct{}{}, stopping: map[string]struct{}{},
		proxies: map[string]map[string]*portProxy{}, paths: map[string]fcInstancePaths{},
	}
	jailDisk := filepath.Join(b.ChrootBase, "firecracker-v1.2", id, "root", "disk.raw")
	for _, f := range []string{jailDisk, filepath.Join(instanceDir(id), "disk.raw")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("disk"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return b, net, filepath.Dir(filepath.Dir(jailDisk))
}

func TestFCReconcileCleansUpAfterAGoneProcess(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	for name, pid := range map[string]int{
		"dead pid":        dead.Process.Pid,
		"not firecracker": os.Getpid(), // alive, but its cmdline is the test binary
	} {
		t.Run(name, func(t *testing.T) {
			b, net, jail := leftoverBackend(t, "R1")
			if err := saveRuntimeState(instanceDir("R1"), runtimeState{Pid: pid, Jailed: true, JailExec: "/usr/bin/firecracker-v1.2"}); err != nil {
				t.Fatal(err)
			}
			spec := &instance.Spec{ID: "R1", Name: "r1", VM: &instance.VMSpec{}}
			state, err := b.Reconcile(context.Background(), spec)
			if err != nil || state != instance.StateStopped {
				t.Fatalf("Reconcile = %s, %v; want stopped", state, err)
			}
			if _, found, _ := loadRuntimeState(instanceDir("R1")); found {
				t.Error("runtime state kept")
			}
			if _, err := os.Stat(jail); !os.IsNotExist(err) {
				t.Errorf("jail dir %s kept (its disk hard link holds the disk's space)", jail)
			}
			if !slices.Contains(net.detached, "R1") {
				t.Errorf("tap not detached: %v", net.detached)
			}
		})
	}
}

func TestFCDeleteRemovesLeftoverJail(t *testing.T) {
	b, net, jail := leftoverBackend(t, "D1")
	if err := b.Delete(context.Background(), &instance.Spec{ID: "D1", Name: "d1", VM: &instance.VMSpec{}}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{jail, instanceDir("D1")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s kept after Delete", dir)
		}
	}
	if !slices.Contains(net.detached, "D1") {
		t.Errorf("tap not detached: %v", net.detached)
	}
}

// qemuLeftoverBackend is a QEMU Backend with no QEMU and a bridged spec for id.
func qemuLeftoverBackend(t *testing.T, id string) (*Backend, *recordingNetworker, *instance.Spec) {
	t.Helper()
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })
	net := &recordingNetworker{}
	b := &Backend{
		Networker: net, running: map[string]*qemu.Process{}, starting: map[string]struct{}{},
		stopping: map[string]struct{}{}, virtiofs: map[string]map[string]*qemu.Virtiofsd{},
	}
	spec := &instance.Spec{ID: id, Name: id, VM: &instance.VMSpec{NetworkMode: "bridge", DiskPath: filepath.Join(root, id, "disk.qcow2")}}
	return b, net, spec
}

// TestQEMUReconcileRemovesLeftoverTap covers a QEMU that died while anvild was down:
// its persistent tap would make the next Start fail.
func TestQEMUReconcileRemovesLeftoverTap(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	b, net, spec := qemuLeftoverBackend(t, "Q1")
	if err := os.MkdirAll(instanceDir("Q1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := saveRuntimeState(instanceDir("Q1"), runtimeState{Pid: dead.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	state, err := b.Reconcile(context.Background(), spec)
	if err != nil || state != instance.StateStopped {
		t.Fatalf("Reconcile = %s, %v; want stopped", state, err)
	}
	if _, found, _ := loadRuntimeState(instanceDir("Q1")); found {
		t.Error("runtime state kept")
	}
	if !slices.Contains(net.detached, "Q1") {
		t.Errorf("tap not detached: %v", net.detached)
	}
}

func TestQEMUDeleteRemovesLeftoverTap(t *testing.T) {
	b, net, spec := qemuLeftoverBackend(t, "Q2")
	if err := b.Delete(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(net.detached, "Q2") {
		t.Errorf("tap not detached: %v", net.detached)
	}
}

// TestQEMUReconcileKillsAnUncontrollableQEMU covers a live QEMU whose QMP is gone:
// left running untracked, a later Start would open its disk in a second QEMU.
func TestQEMUReconcileKillsAnUncontrollableQEMU(t *testing.T) {
	b, net, spec := qemuLeftoverBackend(t, "Q3")
	if err := os.MkdirAll(instanceDir("Q3"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Looks like qemu-system for the disk; the trailing ":" keeps sh from exec'ing sleep.
	fake := exec.Command("sh", "-c", "sleep 60; :", "qemu-system-x86_64", spec.VM.DiskPath)
	fake.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := fake.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = fake.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-fake.Process.Pid, syscall.SIGKILL) })

	rt := runtimeState{Pid: fake.Process.Pid, QMPSocket: filepath.Join(instanceDir("Q3"), "qmp.sock")}
	if err := saveRuntimeState(instanceDir("Q3"), rt); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if state, err := b.Reconcile(ctx, spec); err == nil || state != instance.StateError {
		t.Fatalf("Reconcile = %s, %v; want an error state", state, err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the uncontrollable QEMU was left running")
	}
	if !slices.Contains(net.detached, "Q3") {
		t.Errorf("tap not detached: %v", net.detached)
	}
}
