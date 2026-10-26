package instance

import (
	"context"
	"strings"
	"testing"
)

func TestParseDevice(t *testing.T) {
	ok := map[string]string{
		"usb:046D:C52B":    "usb:046d:c52b",
		"usb:3-2":          "usb:3-2",
		" usb:3-2.1.4 ":    "usb:3-2.1.4",
		"pci:0000:01:00.0": "pci:0000:01:00.0",
		"PCI:0000:0A:1f.7": "pci:0000:0a:1f.7",
	}
	for in, want := range ok {
		got, err := ParseDevice(in)
		if err != nil || got != want {
			t.Errorf("ParseDevice(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "usb", "usb:046d", "usb:3", "usb:3-", "pci:01:00.0", "pci:0000:01:00.8", "scsi:0:0", "usb:3-2:1.0"} {
		if _, err := ParseDevice(in); err == nil {
			t.Errorf("ParseDevice(%q) succeeded, want an error", in)
		}
	}
}

func TestLaunchChecksDevices(t *testing.T) {
	m := NewManager(&memRegistry{}, map[Kind]Backend{KindVM: &plainBackend{}})
	launch := func(v *VMSpec) error {
		return m.Launch(context.Background(), LaunchParams{Name: "dev", Kind: KindVM, VM: v}, func(LaunchEvent) {})
	}
	if err := launch(&VMSpec{Hypervisor: VMHypervisorFirecracker, Devices: []string{"usb:3-2"}}); err == nil || !strings.Contains(err.Error(), "qemu") {
		t.Errorf("firecracker with devices: got %v, want a qemu-only error", err)
	}
	if err := launch(&VMSpec{Devices: []string{"usb:3-2", "USB:3-2"}}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate device: got %v", err)
	}
	v := &VMSpec{Devices: []string{"PCI:0000:01:00.0"}}
	if err := launch(v); err != nil {
		t.Fatalf("valid launch: %v", err)
	}
	if v.Devices[0] != "pci:0000:01:00.0" {
		t.Errorf("devices not normalized: %v", v.Devices)
	}
}

func TestForkRefusesPassthroughVM(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "A", Name: "src", Kind: KindVM, VM: &VMSpec{Devices: []string{"usb:3-2"}}})
	m := NewManager(reg, map[Kind]Backend{KindVM: &plainBackend{}})
	err := m.Fork(context.Background(), ForkParams{Source: "src", NewName: "copy"}, func(LaunchEvent) {})
	if err == nil || !strings.Contains(err.Error(), "can't be forked") {
		t.Errorf("got %v, want a refusal", err)
	}
}
