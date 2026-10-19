//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
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
