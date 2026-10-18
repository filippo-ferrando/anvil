package tui

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestLaunchFieldsGuestToggles(t *testing.T) {
	vm := newSimpleForm("", launchFields("vm", nil))
	if !vm.Bool(guestAgentField) {
		t.Error("expected the guest agent toggle to default to on")
	}
	if vm.Bool(waitField) {
		t.Error("expected the wait toggle to default to off")
	}
	if got := vm.Value(hypervisorField); got != "qemu" {
		t.Errorf("hypervisor defaults to %q, want qemu", got)
	}

	for _, f := range launchFields("container", nil) {
		if f.Label == guestAgentField || f.Label == waitField || f.Label == hypervisorField {
			t.Errorf("expected no %q field for a container", f.Label)
		}
	}
}

func TestParseHypervisorField(t *testing.T) {
	for in, want := range map[string]anvilv1.VmHypervisor{
		"":              anvilv1.VmHypervisor_VM_HYPERVISOR_QEMU,
		"qemu":          anvilv1.VmHypervisor_VM_HYPERVISOR_QEMU,
		" firecracker ": anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER,
	} {
		if got, err := parseHypervisorField(in); err != nil || got != want {
			t.Errorf("parseHypervisorField(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseHypervisorField("xen"); err == nil {
		t.Error("expected an error for an unknown hypervisor")
	}
}
