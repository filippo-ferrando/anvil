package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

	for _, f := range launchFields("vm", nil) {
		if f.Label == entrypointField || f.Label == commandField || f.Label == "Env" {
			t.Errorf("expected no %q field for a VM", f.Label)
		}
	}
	container := newSimpleForm("", launchFields("container", nil))
	for _, f := range container.fields {
		switch f.Label {
		case guestAgentField, hypervisorField, sshKeysField, "CPUs", "Memory MiB", "Disk GiB":
			t.Errorf("expected no %q field for a container", f.Label)
		}
	}
	if !container.Bool(startField) || container.Bool(waitField) {
		t.Error("expected Start on and Wait off for a container too")
	}
}

func TestBuildLaunchRequestContainer(t *testing.T) {
	m := newLaunchModel()
	m.kind = "container"
	m.form.fields = launchFields("container", nil)
	setField(&m.form, "Image", "nginx:alpine")
	setField(&m.form, entrypointField, "/bin/sh")
	setField(&m.form, commandField, "-c  'sleep 1'")
	setField(&m.form, waitTimeoutField, "90s")
	toggle(&m.form, startField)

	req, err := buildLaunchRequest(m)
	if err != nil {
		t.Fatal(err)
	}
	c := req.GetContainer()
	if req.GetKind() != anvilv1.Kind_KIND_CONTAINER || !req.GetNoStart() || req.GetWaitTimeoutSeconds() != 90 {
		t.Errorf("unexpected request %+v", req)
	}
	if strings.Join(c.GetEntrypoint(), "|") != "/bin/sh" || strings.Join(c.GetCmd(), "|") != "-c|'sleep|1'" {
		t.Errorf("entrypoint %q, cmd %q", c.GetEntrypoint(), c.GetCmd())
	}
	if req.GetVm() != nil {
		t.Error("expected no VM spec for a container")
	}

	toggle(&m.form, waitField)
	if _, err := buildLaunchRequest(m); err == nil {
		t.Error("expected wait without start to fail")
	}
	toggle(&m.form, startField)
	if req, err = buildLaunchRequest(m); err != nil || !req.GetWait() || req.GetNoStart() {
		t.Errorf("expected a waiting started launch, got %+v, %v", req, err)
	}

	setField(&m.form, waitTimeoutField, "soon")
	if _, err := buildLaunchRequest(m); err == nil {
		t.Error("expected a bad wait timeout to fail")
	}
}

func TestBuildLaunchRequestVMKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pubFile := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(pubFile, []byte("ssh-ed25519 BBBB file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newLaunchModel()
	setField(&m.form, "Image", "debian:12")
	setField(&m.form, "CPUs", "2")
	setField(&m.form, sshKeysField, "ssh-ed25519 AAAA me, "+pubFile)

	req, err := buildLaunchRequest(m)
	if err != nil {
		t.Fatal(err)
	}
	keys := req.GetVm().GetSshPublicKeys()
	if len(keys) != 3 || keys[1] != "ssh-ed25519 AAAA me" || keys[2] != "ssh-ed25519 BBBB file" {
		t.Errorf("keys = %q, want anvil's own key plus the two extra ones", keys)
	}
	if req.GetVm().GetCpus() != 2 || req.GetNoStart() || req.GetWaitTimeoutSeconds() != 900 {
		t.Errorf("unexpected request %+v", req)
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
		{Id: "usb:3-2.1", Bus: "usb", Name: "Keyboard"},
		{Id: "pci:0000:01:00.0", Bus: "pci", InUseBy: "web"},
		{Id: "pci:0000:00:02.0", Bus: "pci", BlockedReason: "drives the host display"},
	}})
	l := next.(model).launch
	pickerAt := func(label string) int {
		for i, f := range l.form.fields {
			if f.Label == label && f.Kind == fieldPicker {
				return i
			}
		}
		return -1
	}
	usb, pci := pickerAt(usbDevicesField), pickerAt(pciDevicesField)
	if usb < 0 || pci < 0 {
		t.Fatal("expected a USB and a PCI drop-down")
	}
	if n := len(l.form.fields[pci].options); n != 2 {
		t.Fatalf("PCI drop-down lists %d devices, want both unavailable ones", n)
	}

	key := func(k string) {
		var msg tea.KeyMsg
		switch k {
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		}
		l.form, _, _ = l.form.update(msg)
		l.syncDeviceFields()
	}
	// Open USB, check the keyboard, close.
	l.form.focus = usb
	key(" ")
	key(" ")
	key("enter")
	// Open PCI and try to check a device that is in use: it must stay unchecked.
	l.form.focus = pci
	key(" ")
	if !l.form.fields[pci].open {
		t.Fatal("space should open the drop-down")
	}
	if v := l.form.View(); !strings.Contains(v, "in use by web") || !strings.Contains(v, "blocked: drives the host display") {
		t.Errorf("open drop-down should show why devices are unavailable:\n%s", v)
	}
	key(" ")
	key("down")
	key(" ")

	l.form.fields[1].input.SetValue("img")
	req, err := buildLaunchRequest(l)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.GetVm().GetDevices(); len(got) != 1 || got[0] != "usb:3-2.1" {
		t.Errorf("request devices = %v", got)
	}

	// Firecracker hides the drop-downs, switching back restores the pick.
	setHypervisor := func(v string) {
		for i := range l.form.fields {
			if l.form.fields[i].Label == hypervisorField {
				l.form.fields[i].input.SetValue(v)
			}
		}
		l.syncDeviceFields()
	}
	setHypervisor("firecracker")
	if pickerAt(usbDevicesField) >= 0 {
		t.Fatal("expected no device drop-downs for firecracker")
	}
	setHypervisor("qemu")
	if got := l.form.Picked(usbDevicesField); len(got) != 1 {
		t.Error("expected the pick to survive the hypervisor switch")
	}
}
