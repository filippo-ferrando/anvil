//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/cloudinit"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

// TestGuestAgentRealBoot boots a real cloud image with anvil's guest agent setup and a
// boot-time virtiofs mount, then checks cloud-init waiting, guest IPs, live mount/umount,
// and that a guest poweroff is noticed. Needs network access for the package install,
// so it only runs when ANVIL_E2E_IMAGE points at a cloud image qcow2.
func TestGuestAgentRealBoot(t *testing.T) {
	baseImage := os.Getenv("ANVIL_E2E_IMAGE")
	if baseImage == "" {
		t.Skip("ANVIL_E2E_IMAGE not set, skipping")
	}
	for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "xorriso"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
	if _, err := qemu.VirtiofsdPath(); err != nil {
		t.Skip("virtiofsd not installed, skipping")
	}

	// Short paths: unix sockets live here and are limited to 108 bytes.
	root, err := os.MkdirTemp("", "anvil-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })

	const id = "E2E"
	dir := instanceDir(id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	share := func(name, content string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "hello.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	disk := filepath.Join(dir, "disk.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", baseImage, disk, "10G")

	bootMount := instance.Mount{HostPath: share("boot", "from boot mount\n"), GuestPath: "/mnt/boot", Tag: "mount0"}
	spec := &instance.Spec{ID: id, Name: "e2e", VM: &instance.VMSpec{
		Mounts: []instance.Mount{bootMount}, NextMountIndex: 1, MountFS: mountFSVirtiofs,
	}}

	userData, err := mergeGuestAgent("#cloud-config\n{}\n")
	if err != nil {
		t.Fatal(err)
	}
	if userData, err = mergeMounts(userData, spec.VM.Mounts, 0); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(dir, "seed.iso")
	seed := cloudinit.Seed{UserData: userData, MetaData: "instance-id: e2e\nlocal-hostname: e2e\n"}
	if err := cloudinit.NewBuilder().Build(seed, seedPath); err != nil {
		t.Fatal(err)
	}
	spec.VM.SeedISOPath = seedPath

	b := NewBackend(nil, nil, noMirrors{})
	exits := make(chan instance.State, 1)
	b.SetExitHook(func(_ string, state instance.State) { exits <- state })

	mounts, err := b.startVirtiofsds(spec)
	if err != nil {
		t.Fatalf("startVirtiofsds: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cfg := qemu.Config{
		CPUs: 2, MemoryMiB: 2048, DiskPath: disk, SeedISOPath: seedPath,
		QMPSocket:        filepath.Join(dir, "qmp.sock"),
		SerialLogPath:    filepath.Join(dir, "console.log"),
		GuestAgentSocket: guestAgentSocket(id),
		KVM:              kvmAvailable(),
		Disk:             qemu.ProbeDiskTuning(ctx, disk),
		Mounts:           mounts,
	}
	proc, err := qemu.Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		_ = proc.Stop(context.Background(), 0)
		_ = proc.Close()
		b.killVirtiofsds(id)
		if t.Failed() {
			for _, f := range []string{"console.log", "qemu.log"} {
				data, _ := os.ReadFile(filepath.Join(dir, f))
				t.Logf("%s tail:\n%s", f, tail(string(data), 3000))
			}
		}
	}()
	if err := proc.AttachQMP(ctx); err != nil {
		t.Fatalf("AttachQMP: %v", err)
	}
	b.running[id] = proc
	b.guests[id] = &guestState{agent: qemu.NewGuestAgent(cfg.GuestAgentSocket), cancel: func() {}}
	go b.watchExit(id, "", proc)

	start := time.Now()
	elapsed := func() time.Duration { return time.Since(start).Round(time.Second) }
	if err := b.WaitReady(ctx, spec, func(s string) { t.Logf("%s: %s", elapsed(), s) }); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	info, _ := b.GuestInfo(spec)
	t.Logf("%s: guest ready: %+v", elapsed(), info)
	if !info.AgentConnected || len(info.IPAddresses) == 0 {
		t.Fatalf("expected a connected agent with IPs, got %+v", info)
	}

	guest := func(script string) (string, error) {
		res, err := b.guestExec(ctx, id, script)
		return strings.TrimSpace(res.Stdout), err
	}
	mustGuest := func(script string) string {
		t.Helper()
		out, err := guest(script)
		if err != nil {
			t.Fatalf("guest %q: %v", script, err)
		}
		return out
	}

	if got := mustGuest("cat /mnt/boot/hello.txt"); got != "from boot mount" {
		t.Errorf("boot-time mount: got %q", got)
	}

	liveDir := share("live", "from live mount\n")
	if err := b.Mount(ctx, spec, liveDir, "/mnt/live", false); err != nil {
		t.Fatalf("live Mount: %v", err)
	}
	if b.running[id] != proc {
		t.Fatal("expected the live mount not to restart the VM")
	}
	t.Logf("%s: live mount added", elapsed())
	if got := mustGuest("cat /mnt/live/hello.txt"); got != "from live mount" {
		t.Errorf("live mount read: got %q", got)
	}
	mustGuest("echo from guest > /mnt/live/guest.txt")
	if data, err := os.ReadFile(filepath.Join(liveDir, "guest.txt")); err != nil || string(data) != "from guest\n" {
		t.Errorf("expected the guest's write on the host, got %q, %v", data, err)
	}

	roDir := share("ro", "read only\n")
	if err := b.Mount(ctx, spec, roDir, "/mnt/ro", true); err != nil {
		t.Fatalf("read-only Mount: %v", err)
	}
	if _, err := guest("touch /mnt/ro/nope"); err == nil {
		t.Error("expected a write to the read-only mount to fail")
	}

	for _, p := range []string{"/mnt/live", "/mnt/boot"} {
		if err := b.Umount(ctx, spec, p); err != nil {
			t.Fatalf("live Umount %s: %v", p, err)
		}
		if _, err := guest("mountpoint -q " + p); err == nil {
			t.Errorf("expected %s to be unmounted", p)
		}
	}
	t.Logf("%s: live umounts done", elapsed())
	fstab := mustGuest("cat /etc/fstab")
	if strings.Contains(fstab, "/mnt/live") || strings.Contains(fstab, "/mnt/boot") || !strings.Contains(fstab, "/mnt/ro") {
		t.Errorf("unexpected fstab after umounts:\n%s", fstab)
	}
	if len(spec.VM.Mounts) != 1 || spec.VM.Mounts[0].GuestPath != "/mnt/ro" {
		t.Errorf("expected only /mnt/ro left in the spec, got %+v", spec.VM.Mounts)
	}

	// A poweroff from inside the guest, not through Stop.
	_, _ = guest("systemctl poweroff")
	select {
	case state := <-exits:
		if state != instance.StateStopped {
			t.Errorf("expected the guest poweroff to be reported as stopped, got %s", state)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("guest poweroff was never reported")
	}
	t.Logf("%s: guest poweroff noticed", elapsed())
	b.mu.Lock()
	leftover := len(b.virtiofs[id])
	b.mu.Unlock()
	if leftover != 0 {
		t.Errorf("expected every virtiofsd to be released, %d left", leftover)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
