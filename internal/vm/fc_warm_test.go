//go:build linux

package vm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
)

func TestWarmSpecKey(t *testing.T) {
	if _, err := firecracker.LookPath(firecracker.Binary); err != nil {
		t.Skip("firecracker not installed, its version is part of the key")
	}
	b := fcTestBackend(t)
	guest := t.TempDir()
	t.Setenv("ANVIL_GUEST_TOOLS", guest)
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	for path, body := range map[string]string{filepath.Join(guest, guestBinary): "v1", kernel: "k1"} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANVIL_FC_KERNEL", kernel)

	if _, err := b.WarmSpec(ephemeral.WarmParams{Distro: "fc-test"}); !errors.Is(err, errNeedsJailer) {
		t.Fatalf("WarmSpec without the jailer = %v, want errNeedsJailer", err)
	}
	b.unjailedClones = true
	base, err := b.WarmSpec(ephemeral.WarmParams{Distro: "fc-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Key) != 16 || base.Distro != "fc-test" || base.CPUs != ephemeral.DefaultCPUs || base.MemoryMiB != ephemeral.DefaultMemoryMiB ||
		base.DiskGiB != warmDiskGiB || !strings.HasPrefix(base.FirecrackerVersion, "Firecracker") {
		t.Errorf("spec = %+v", base)
	}
	if again, _ := b.WarmSpec(ephemeral.WarmParams{Distro: "fc-test", CPUs: ephemeral.DefaultCPUs}); again.Key != base.Key {
		t.Errorf("the defaults spelled out changed the key: %s vs %s", again.Key, base.Key)
	}
	keys := map[string]string{base.Key: "base"}
	differ := func(what string) {
		t.Helper()
		s, err := b.WarmSpec(ephemeral.WarmParams{Distro: "fc-test", MemoryMiB: map[bool]int64{true: 1024}[what == "memory"],
			CPUs: map[bool]int{true: 4}[what == "cpus"]})
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := keys[s.Key]; dup {
			t.Errorf("%s gave the same key as %s", what, other)
		}
		keys[s.Key] = what
	}
	differ("cpus")
	differ("memory")
	if err := os.WriteFile(filepath.Join(guest, guestBinary), []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	differ("anvil-guest")
	if err := os.WriteFile(kernel, []byte("k2"), 0o644); err != nil {
		t.Fatal(err)
	}
	differ("kernel")

	if _, err := b.WarmSpec(ephemeral.WarmParams{Distro: "qemu-only"}); err == nil {
		t.Error("a distro without a firecracker entry got a warm spec")
	}
	_ = os.Remove(filepath.Join(guest, guestBinary))
	if _, err := b.WarmSpec(ephemeral.WarmParams{Distro: "fc-test"}); err == nil || !strings.Contains(err.Error(), guestBinary) {
		t.Errorf("WarmSpec without anvil-guest = %v", err)
	}
}

func TestWarmVMSpec(t *testing.T) {
	v := warmVMSpec(ephemeral.WarmSpec{Distro: "d", CPUs: 2, MemoryMiB: 512, DiskGiB: 10}, "user")
	if standalone(v) || !v.AnvilGuest || !v.NoGuestAgent || v.MACAddress != warmMAC {
		t.Errorf("warm VM spec = %+v", v)
	}
	cfg := bridgeNetworkConfig(v, v.MACAddress)
	for _, want := range []string{"10.200.0.2/30", "10.200.0.1", warmMAC} {
		if !strings.Contains(cfg, want) {
			t.Errorf("network-config lacks %s:\n%s", want, cfg)
		}
	}
}
