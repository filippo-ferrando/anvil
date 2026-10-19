//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// TestFirecrackerRealBoot boots the cloud image in ANVIL_E2E_IMAGE (a qcow2) under
// firecracker with the kernel in ANVIL_FC_KERNEL, through FCBackend: create, start,
// cloud-init done (read from the console, the VM has no network yet), stats, a stop
// through Ctrl+Alt+Del, and a reconcile of a VM left running. ANVIL_FC_KERNEL_ARGS adds
// kernel arguments, e.g. rootflags=subvol=root for Fedora.
func TestFirecrackerRealBoot(t *testing.T) {
	baseImage, kernel := os.Getenv("ANVIL_E2E_IMAGE"), os.Getenv(image.KernelEnv)
	if baseImage == "" || kernel == "" {
		t.Skipf("ANVIL_E2E_IMAGE and %s not both set, skipping", image.KernelEnv)
	}
	for _, bin := range []string{"firecracker", "qemu-img", "xorriso"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}

	// Short paths: unix sockets live here and are limited to 108 bytes.
	root, err := os.MkdirTemp("", "anvil-fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })

	base, err := image.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := base.WithMirrors([]image.MirrorManifest{{ManifestJSON: `{"schema_version": 1,
		"distros": [{"id": "e2e", "arch": "x86_64", "url": "http://unused.invalid/"}],
		"firecracker": {"distros": [{"id": "e2e", "kernel_args": "` + os.Getenv("ANVIL_FC_KERNEL_ARGS") + `"}]}}`}})
	if err != nil {
		t.Fatal(err)
	}
	vault := image.NewVault(filepath.Join(root, "prepared"))
	entry, _ := catalog.Find("e2e", "")
	cached, _ := vault.CachedPath(entry)
	if err := os.MkdirAll(filepath.Dir(cached), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(baseImage, cached); err != nil {
		t.Fatal(err)
	}
	b := NewFCBackend(NewBackend(catalog, vault, noMirrors{}), filepath.Join(root, "fc"))
	exits := make(chan instance.State, 1)
	b.SetExitHook(func(_ string, state instance.State, _ string) { exits <- state })

	const id = "FCE2E"
	spec := &instance.Spec{ID: id, Name: "fc-e2e", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "e2e", CPUs: 2, MemoryMiB: 1024, DiskGiB: 10, NoGuestAgent: true,
		Hypervisor: instance.VMHypervisorFirecracker,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	defer func() {
		_ = b.Delete(context.Background(), spec)
		if t.Failed() {
			for _, f := range []string{"console.log", "firecracker.log"} {
				data, _ := os.ReadFile(filepath.Join(instanceDir(id), f))
				t.Logf("%s tail:\n%s", f, tail(string(data), 3000))
			}
		}
	}()

	if err := b.Create(ctx, spec, func(s string) { t.Log(s) }); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Start(ctx, spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st, err := b.Status(ctx, spec); err != nil || st != instance.StateRunning {
		t.Fatalf("Status = %v, %v; want running", st, err)
	}
	if err := b.WaitReady(ctx, spec, func(s string) { t.Log(s) }); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	t.Log("cloud-init finished")
	if stats, err := b.Stats(ctx, spec); err != nil || stats.MemUsedBytes == 0 {
		t.Errorf("Stats = %+v, %v", stats, err)
	}

	// A new backend, as after an anvild restart, takes the running VM over.
	b2 := NewFCBackend(b.QEMU, b.Cache.Dir)
	if st, err := b2.Reconcile(ctx, spec); err != nil || st != instance.StateRunning {
		t.Fatalf("Reconcile = %v, %v; want running", st, err)
	}
	b2.mu.Lock()
	delete(b2.running, id) // b stays the one that stops it
	b2.mu.Unlock()

	b.mu.Lock()
	proc := b.running[id]
	b.mu.Unlock()
	start := time.Now()
	if err := b.Stop(ctx, spec, false, 60*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// A kill shows up as a signal; Ctrl+Alt+Del and the guest's reboot exit cleanly.
	if !proc.ExitedCleanly() {
		t.Errorf("firecracker ended with %s after %s: the guest ignored Ctrl+Alt+Del", proc.ExitStatus(), time.Since(start))
	}
	t.Logf("stopped through Ctrl+Alt+Del in %s", time.Since(start).Round(time.Millisecond))
	select {
	case st := <-exits:
		t.Errorf("Stop was reported as an outside exit (%s)", st)
	default:
	}
}
