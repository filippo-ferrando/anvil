package tui

import (
	"strings"
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

func TestLaunchImageSuggestionsFollowHypervisor(t *testing.T) {
	m := model{screen: screenLaunch, launch: newLaunchModel()}
	next, _ := m.Update(catalogLoadedMsg{entries: []*anvilv1.CatalogEntry{
		{Id: "ubuntu:24.04", Firecracker: true}, {Id: "alpine:3.20"},
	}})
	next, _ = next.(model).Update(cachedImagesLoadedMsg{images: []*anvilv1.CachedImage{
		{Id: "debian:12", Kind: "base"}, {Id: "debian:12", Kind: "firecracker-raw"},
		{Id: "anvil-fc-kernel-6.18-x86_64", Kind: "firecracker-kernel"},
	}})
	s := next.(model).launch.suggest
	if got := strings.Join(s.imagesFor("vm", "qemu"), ","); got != "ubuntu:24.04,alpine:3.20,debian:12" {
		t.Errorf("qemu suggestions = %q", got)
	}
	if got := strings.Join(s.imagesFor("vm", "firecracker"), ","); got != "ubuntu:24.04,debian:12" {
		t.Errorf("firecracker suggestions = %q", got)
	}
}

func TestLaunchDevicePicker(t *testing.T) {
	m := model{screen: screenLaunch, launch: newLaunchModel()}
	next, _ := m.Update(hostDevicesLoadedMsg{devices: []*anvilv1.HostDevice{
		{Id: "usb:3-2.1", Name: "Keyboard"},
		{Id: "pci:0000:01:00.0", InUseBy: "web"},
		{Id: "pci:0000:00:02.0", BlockedReason: "drives the host display"},
	}})
	l := next.(model).launch
	var toggles []string
	for i, f := range l.form.fields {
		if id, ok := strings.CutPrefix(f.Label, devicePrefix); ok {
			toggles = append(toggles, id)
			l.form.fields[i].on = true
		}
	}
	if strings.Join(toggles, ",") != "usb:3-2.1" {
		t.Fatalf("picker offers %v, want only the free device", toggles)
	}
	if un := l.unavailableDevices(); len(un) != 2 {
		t.Errorf("unavailable = %v, want the in-use and blocked devices", un)
	}

	l.form.fields[1].input.SetValue("img")
	req, err := buildLaunchRequest(l)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.GetVm().GetDevices(); len(got) != 1 || got[0] != "usb:3-2.1" {
		t.Errorf("request devices = %v", got)
	}

	// Firecracker hides the picker, switching back restores the pick.
	setHypervisor := func(v string) {
		for i := range l.form.fields {
			if l.form.fields[i].Label == hypervisorField {
				l.form.fields[i].input.SetValue(v)
			}
		}
		l.syncDeviceFields()
	}
	setHypervisor("firecracker")
	for _, f := range l.form.fields {
		if strings.HasPrefix(f.Label, devicePrefix) {
			t.Fatal("expected no device toggles for firecracker")
		}
	}
	setHypervisor("qemu")
	if !l.form.Bool(devicePrefix + "usb:3-2.1") {
		t.Error("expected the pick to survive the hypervisor switch")
	}
}
