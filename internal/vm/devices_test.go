//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

// fakeClaimer records claims and releases; claims of fail error out.
type fakeClaimer struct {
	claimed, released []string
	fail              string
}

func (f *fakeClaimer) Claim(_ context.Context, bus, addr string) (hostdev.Claim, error) {
	if addr == f.fail {
		return hostdev.Claim{}, errors.New("helper said no")
	}
	f.claimed = append(f.claimed, addr)
	members := []hostdev.Member{{Addr: addr}}
	if bus == "pci" {
		members = append(members, hostdev.Member{Addr: strings.TrimSuffix(addr, "0") + "1"})
	}
	return hostdev.Claim{Bus: bus, Addr: addr, Members: members}, nil
}

func (f *fakeClaimer) Release(_ context.Context, cl hostdev.Claim) error {
	f.released = append(f.released, cl.Addr)
	return nil
}

func deviceBackend(t *testing.T) (*Backend, *fakeClaimer) {
	t.Helper()
	origDir, origResolve := instanceDir, resolveDevice
	tmp := t.TempDir()
	instanceDir = func(id string) string { return filepath.Join(tmp, id) }
	resolveDevice = func(id string) (string, string, []string, error) {
		bus, addr, _ := strings.Cut(id, ":")
		if bus == "pci" {
			return bus, addr, []string{addr, strings.TrimSuffix(addr, "0") + "1"}, nil
		}
		return bus, addr, []string{addr}, nil
	}
	t.Cleanup(func() { instanceDir, resolveDevice = origDir, origResolve })
	b := NewBackend(nil, nil, noMirrors{})
	fc := &fakeClaimer{}
	b.Devices = fc
	return b, fc
}

func devSpec(id string, devices ...string) *instance.Spec {
	return &instance.Spec{ID: id, VM: &instance.VMSpec{Devices: devices}}
}

func TestClaimDevicesFillsConfigAndHolds(t *testing.T) {
	b, _ := deviceBackend(t)
	var cfg qemu.Config
	claims, err := b.claimDevices(context.Background(), devSpec("A", "usb:3-2", "pci:0000:04:00.0"), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || !slices.Equal(cfg.USBDevices, []string{"3-2"}) ||
		!slices.Equal(cfg.PCIDevices, []string{"0000:04:00.0", "0000:04:00.1"}) {
		t.Errorf("claims %v, cfg usb %v pci %v", claims, cfg.USBDevices, cfg.PCIDevices)
	}
	held := b.HeldDevices()
	if held["3-2"] != "A" || held["0000:04:00.1"] != "A" {
		t.Errorf("held = %v", held)
	}

	// A group member held by A blocks B, and B keeps nothing.
	if _, err := b.claimDevices(context.Background(), devSpec("B", "pci:0000:04:00.0"), &qemu.Config{}); err == nil ||
		!strings.Contains(err.Error(), "another running instance") {
		t.Errorf("second claim: got %v", err)
	}
	if len(b.HeldDevices()) != 3 {
		t.Errorf("held after refused claim = %v", b.HeldDevices())
	}
}

func TestClaimDevicesRollsBackOnFailure(t *testing.T) {
	b, fc := deviceBackend(t)
	fc.fail = "3-3"
	if _, err := b.claimDevices(context.Background(), devSpec("A", "usb:3-2", "usb:3-3"), &qemu.Config{}); err == nil {
		t.Fatal("expected the claim to fail")
	}
	if !slices.Equal(fc.released, []string{"3-2"}) || len(b.HeldDevices()) != 0 {
		t.Errorf("released %v, held %v", fc.released, b.HeldDevices())
	}
}

func TestReleaseRuntimeDevices(t *testing.T) {
	b, fc := deviceBackend(t)
	claims, err := b.claimDevices(context.Background(), devSpec("A", "usb:3-2"), &qemu.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveRuntimeState(mkInstanceDir(t, "A"), runtimeState{Devices: claims}); err != nil {
		t.Fatal(err)
	}
	b.releaseRuntimeDevices("A")
	if !slices.Equal(fc.released, []string{"3-2"}) || len(b.HeldDevices()) != 0 {
		t.Errorf("released %v, held %v", fc.released, b.HeldDevices())
	}
}

func mkInstanceDir(t *testing.T, id string) string {
	t.Helper()
	dir := instanceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
