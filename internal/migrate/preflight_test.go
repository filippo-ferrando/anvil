package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
)

func vmSpec() *instance.Spec {
	return &instance.Spec{
		Name: "web",
		Kind: instance.KindVM,
		VM:   &instance.VMSpec{Arch: "x86_64", MemoryMiB: 2048},
	}
}

func goodVMCaps() remoteCaps {
	return remoteCaps{anvil: "/usr/bin/anvil", anvild: "/usr/bin/anvild", arch: "x86_64", kvm: true, qemu: true, memTotalKB: 16 << 20}
}

func TestPreflightVMArch(t *testing.T) {
	spec := vmSpec()

	caps := goodVMCaps()
	caps.arch, caps.qemu = "aarch64", false
	fatal, _ := preflightVM(spec, caps)
	if len(fatal) != 1 || !strings.Contains(fatal[0], "qemu-system-x86_64") {
		t.Errorf("expected a missing-binary failure, got %v", fatal)
	}

	// Cross-arch with the right qemu installed is allowed, just slow.
	caps.qemu = true
	fatal, warn := preflightVM(spec, caps)
	if len(fatal) != 0 || len(warn) != 1 || !strings.Contains(warn[0], "emulation") {
		t.Errorf("expected only an emulation warning, got fatal=%v warn=%v", fatal, warn)
	}

	caps = goodVMCaps()
	caps.kvm = false
	if _, warn := preflightVM(spec, caps); len(warn) != 1 || !strings.Contains(warn[0], "/dev/kvm") {
		t.Errorf("expected a kvm warning, got %v", warn)
	}

	if fatal, warn := preflightVM(spec, goodVMCaps()); len(fatal) != 0 || len(warn) != 0 {
		t.Errorf("expected a clean check, got fatal=%v warn=%v", fatal, warn)
	}
}

func TestPreflightVMMemoryAndMounts(t *testing.T) {
	spec := vmSpec()
	spec.VM.Mounts = []instance.Mount{{HostPath: t.TempDir(), GuestPath: "/data"}}
	caps := goodVMCaps()
	caps.memTotalKB = 1 << 20 // 1 GiB, less than the VM's 2 GiB

	fatal, warn := preflightVM(spec, caps)
	if len(fatal) != 0 {
		t.Fatalf("neither check should block a migration, got %v", fatal)
	}
	if len(warn) != 2 || !strings.Contains(warn[0], "RAM") || !strings.Contains(warn[1], "shared folder") {
		t.Errorf("expected a memory and a shared folder warning, got %v", warn)
	}
}

func TestPreflightSpaceShortfall(t *testing.T) {
	caps := goodVMCaps()
	caps.freeStagingKB, caps.freeStateKB = 1024, 1<<20 // 1 MiB staging, 1 GiB state

	if short := caps.spaceShortfall(512 << 10); len(short) != 0 {
		t.Errorf("512 KiB fits in both, got %v", short)
	}
	short := caps.spaceShortfall(4 << 20)
	if len(short) != 1 || !strings.Contains(short[0], remoteStagingDir) {
		t.Errorf("expected only the staging dir to be short, got %v", short)
	}
	// Free space the probe couldn't read is never reported as a shortfall.
	if short := (remoteCaps{}).spaceShortfall(1 << 40); len(short) != 0 {
		t.Errorf("unknown free space should be skipped, got %v", short)
	}
}

func TestPreflightContainer(t *testing.T) {
	spec := &instance.Spec{
		Name:      "api",
		Kind:      instance.KindContainer,
		Container: &instance.ContainerSpec{ImageRef: "nginx", Engine: instance.ContainerEngineDocker},
	}
	caps := remoteCaps{anvil: "/usr/bin/anvil", anvild: "/usr/bin/anvild", arch: hostArch()}

	fatal, warn := preflightContainer(spec, caps)
	if len(fatal) != 1 || !strings.Contains(fatal[0], "docker") {
		t.Errorf("expected a missing docker failure, got %v", fatal)
	}
	if len(warn) != 0 {
		t.Errorf("expected no warnings, got %v", warn)
	}

	caps.docker, caps.tar = true, true
	spec.Container.Volumes = []instance.VolumeMount{{HostPath: t.TempDir(), ContainerPath: "/var/lib/db"}}
	fatal, warn = preflightContainer(spec, caps)
	if len(fatal) != 0 {
		t.Fatalf("expected no failures, got %v", fatal)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "travel") {
		t.Errorf("expected a bind mount warning, got %v", warn)
	}
}

// TestProbeRemoteLocally runs the probe script against this machine, which is
// the only way to tell it still works on a real shell.
func TestProbeRemoteLocally(t *testing.T) {
	localSSH(t, nil)
	caps := probeRemote(context.Background(), target{}, vmSpec(), "")
	if caps.arch != hostArch() {
		t.Errorf("probed arch %q, want %q", caps.arch, hostArch())
	}
	if caps.memTotalKB <= 0 {
		t.Errorf("expected a total memory reading, got %d KB", caps.memTotalKB)
	}
	// Free space is left out on purpose: the anvil directories the script reads
	// only exist on a host that has anvil installed.
}

// cannedProbe makes every probe return out instead of running the real script.
func cannedProbe(t *testing.T, out string) {
	localSSH(t, func(string) string { return "printf '%s' " + shQuote(out) })
}

func TestPreflightRejectsUnusableTarget(t *testing.T) {
	m := &Manager{}
	tgt := target{User: "u", Host: "h"}

	cannedProbe(t, "arch=x86_64\n")
	if _, _, err := m.preflight(context.Background(), tgt, vmSpec(), "web"); err == nil ||
		!strings.Contains(err.Error(), "anvil/anvild") {
		t.Errorf("expected a missing anvil failure, got %v", err)
	}

	cannedProbe(t, "arch=x86_64\nanvil=/usr/bin/anvil\nanvild=/usr/bin/anvild\nqemu\nkvm\nnametaken\n")
	if _, _, err := m.preflight(context.Background(), tgt, vmSpec(), "web"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected a name collision failure, got %v", err)
	}

	cannedProbe(t, "arch=x86_64\nanvil=/usr/bin/anvil\nanvild=/usr/bin/anvild\nqemu\nkvm\n")
	if _, _, err := m.preflight(context.Background(), tgt, vmSpec(), "web"); err != nil {
		t.Errorf("expected a usable target, got %v", err)
	}
}
