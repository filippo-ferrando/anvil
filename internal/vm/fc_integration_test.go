//go:build linux

package vm

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// TestFirecrackerRealBoot boots the cloud image in ANVIL_E2E_IMAGE (a qcow2) under
// firecracker with the kernel in ANVIL_FC_KERNEL, through FCBackend: create, start,
// cloud-init done (read from the console), SSH through the port proxy, a live port
// forward, stats, a reconcile of a VM left running and a stop through Ctrl+Alt+Del.
// ANVIL_FC_KERNEL_ARGS adds kernel arguments, e.g. rootflags=subvol=root for Fedora.
// The standalone bridge needs CAP_NET_ADMIN: run it under `unshare -rn` to keep it off
// the host's network.
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

	// The jailer needs real root (mknod is refused in a user namespace): opt in as root.
	if os.Getenv("ANVIL_FC_TEST_JAILER") == "" {
		t.Setenv(fcJailerEnv, "off")
	}
	if err := ensureFCNetwork(); err != nil {
		t.Skipf("can't set up the standalone bridge (needs CAP_NET_ADMIN, try unshare -rn): %v", err)
	}
	// In a fresh network namespace loopback starts down; the proxies listen on it.
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
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
	b := NewFCBackend(NewBackend(catalog, vault, noMirrors{}), filepath.Join(root, "fc"), nil)
	b.ChrootBase = filepath.Join(root, "jailer")
	t.Logf("jailer: %q", b.Jailer)
	exits := make(chan instance.State, 1)
	b.SetExitHook(func(_ string, state instance.State, _ string) { exits <- state })

	// A boot-time NFS mount and the GUI tools drive: the guest copies a tool through the
	// share, so the host sees both worked.
	share := filepath.Join(root, "share")
	tools := filepath.Join(root, "tools")
	for _, d := range []string{share, tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tools, "waypipe"), []byte("fake waypipe\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANVIL_GUI_TOOLS", tools)
	guestScript := "set -e; mkdir -p /opt/anvil; " + fcToolsMount +
		"; cp /opt/anvil/waypipe /mnt/share/waypipe-copy; echo ok > /mnt/share/from-guest"

	const id = "FCE2E"
	spec := &instance.Spec{ID: id, Name: "fc-e2e", Kind: instance.KindVM, GUI: true, VM: &instance.VMSpec{
		ImageRef: "e2e", CPUs: 2, MemoryMiB: 1024, DiskGiB: 10, NoGuestAgent: true,
		Hypervisor:        instance.VMHypervisorFirecracker,
		CloudInitUserData: "#cloud-config\nruncmd:\n  - [sh, -c, " + strconv.Quote(guestScript) + "]\n",
		Mounts:            []instance.Mount{{HostPath: share, GuestPath: "/mnt/share", Tag: "mount0"}},
		NextMountIndex:    1,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	defer func() {
		if t.Failed() {
			for _, f := range []string{"console.log", "firecracker.log"} {
				data, _ := os.ReadFile(filepath.Join(instanceDir(id), f))
				t.Logf("%s tail:\n%s", f, tail(string(data), 3000))
			}
		}
		_ = b.Delete(context.Background(), spec)
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
	for file, want := range map[string]string{"from-guest": "ok\n", "waypipe-copy": "fake waypipe\n"} {
		if data, err := os.ReadFile(filepath.Join(share, file)); err != nil || string(data) != want {
			t.Errorf("%s through the NFS share = %q, %v; want %q", file, data, err, want)
		}
	}
	if stats, err := b.Stats(ctx, spec); err != nil || stats.MemUsedBytes == 0 || !stats.NetAvailable {
		t.Errorf("Stats = %+v, %v", stats, err)
	}

	sshBanner(t, spec.VM.SSHPort)
	free, err := startPortProxy("tcp", 0, "127.0.0.1:1") // borrow a free port number
	if err != nil {
		t.Fatal(err)
	}
	free.Close()
	if err := b.AddPort(ctx, spec, instance.PortMapping{HostPort: free.HostPort, GuestPort: 22}); err != nil {
		t.Fatalf("AddPort: %v", err)
	}
	sshBanner(t, free.HostPort)
	if err := b.RemovePort(ctx, spec, free.HostPort, "tcp"); err != nil {
		t.Fatalf("RemovePort: %v", err)
	}

	// Phase 6 on a running VM: snapshot and fork pause it for a reflink copy.
	if err := b.CreateSnapshot(ctx, spec, "live"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if st, _ := b.Status(ctx, spec); st != instance.StateRunning {
		t.Fatalf("Status after a live snapshot = %v, want running again", st)
	}
	forkClone := *spec.VM
	forkClone.DiskPath, forkClone.SeedISOPath, forkClone.StaticIP, forkClone.MACAddress = "", "", "", ""
	forkClone.NetworkMode = "slirp"
	fork := &instance.Spec{ID: "FCFORK", Name: "fc-fork", Kind: instance.KindVM, VM: &forkClone}
	if err := b.Fork(ctx, spec, fork, nil); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	defer b.Delete(context.Background(), fork)
	if note, err := b.ResizeDisk(ctx, spec, 12); err != nil || !strings.Contains(note, "live") {
		t.Fatalf("ResizeDisk = %q, %v", note, err)
	}
	note, err := b.SetMemoryLive(ctx, spec, spec.VM.MemoryMiB+512)
	if err != nil {
		t.Fatalf("SetMemoryLive: %v", err)
	}
	t.Log(note)
	if err := b.RestoreSnapshot(ctx, spec, "live"); err != nil {
		t.Fatalf("RestoreSnapshot on a running VM: %v", err)
	}
	if st, _ := b.Status(ctx, spec); st != instance.StateRunning {
		t.Fatalf("Status after restore = %v, want running", st)
	}
	sshBanner(t, spec.VM.SSHPort)

	// A new backend, as after an anvild restart, takes the running VM over.
	// Its proxies and mount server died with the old anvild, so b's go first.
	b.closeProxies(id)
	b.unexportMounts(id)
	for _, l := range b.nfs.listeners {
		l.Close()
	}
	b2 := NewFCBackend(b.QEMU, b.Cache.Dir, nil)
	b2.ChrootBase = b.ChrootBase
	if st, err := b2.Reconcile(ctx, spec); err != nil || st != instance.StateRunning {
		t.Fatalf("Reconcile = %v, %v; want running", st, err)
	}
	sshBanner(t, spec.VM.SSHPort)
	b2.closeProxies(id)
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

	// Phase 8: the disk travels as a qcow2 delta and boots again as a migrated VM, which keeps
	// its address (the source is deleted first, as after a migration).
	sum, err := b.BaseImageChecksum(spec)
	if err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(root, "migrated.qcow2")
	if err := b.ExportDiskDelta(ctx, spec, delta); err != nil {
		t.Fatalf("ExportDiskDelta: %v", err)
	}
	addr := spec.VM.StaticIP
	if err := b.Delete(ctx, spec); err != nil {
		t.Fatal(err)
	}
	migrated := &instance.Spec{ID: "FCMIG", Name: "fc-mig", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "e2e", CPUs: 1, MemoryMiB: 1024, Hypervisor: instance.VMHypervisorFirecracker,
		SourceDiskPath: delta, SourceDiskBaseSHA256: sum, StaticIP: addr, MACAddress: spec.VM.MACAddress,
	}}
	defer b.Delete(context.Background(), migrated)
	if err := b.Create(ctx, migrated, nil); err != nil {
		t.Fatalf("Create from the migrated disk: %v", err)
	}
	if err := b.Start(ctx, migrated); err != nil {
		t.Fatalf("Start of the migrated VM: %v", err)
	}
	if migrated.VM.StaticIP != addr {
		t.Fatalf("migrated VM got %s, want its old %s", migrated.VM.StaticIP, addr)
	}
	sshBanner(t, migrated.VM.SSHPort)
	if err := b.Stop(ctx, migrated, false, 60*time.Second); err != nil {
		t.Fatalf("Stop of the migrated VM: %v", err)
	}
	t.Log("migrated disk booted with its old address")
}

// sshBanner checks the guest's sshd answers through host port, retrying while it starts.
func sshBanner(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 64)
			n, _ := conn.Read(buf)
			conn.Close()
			if strings.HasPrefix(string(buf[:n]), "SSH-2.0") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no SSH banner through port %d: %v", port, err)
		}
		time.Sleep(time.Second)
	}
}
