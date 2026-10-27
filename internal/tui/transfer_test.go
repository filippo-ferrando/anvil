package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func TestTransferPromptFields(t *testing.T) {
	vm := instancesModelWith(&anvilv1.Instance{Name: "web", Vm: &anvilv1.VMSpec{}})
	vm.instances.lastIdentity = "/k"
	next, _ := vm.Update(key("t"))
	form := next.(model).instances.promptForm
	if next.(model).instances.prompt != instancesPromptTransfer || form.Value(identityLabel) != "/k" || !hasField(form, transferUserField) {
		t.Fatalf("VM transfer prompt: %+v", form.fields)
	}
	ct := instancesModelWith(&anvilv1.Instance{Name: "nginx", Container: &anvilv1.ContainerSpec{}})
	next, _ = ct.Update(key("t"))
	if form := next.(model).instances.promptForm; hasField(form, identityLabel) || hasField(form, transferUserField) {
		t.Error("expected no user or identity field for a container")
	}
}

func TestTransferFrom(t *testing.T) {
	form := newSimpleForm("t", transferFields(&anvilv1.Instance{Name: "web"}, instancesModel{}))
	if _, err := transferFrom(form, "web"); err == nil {
		t.Error("expected empty paths to fail")
	}
	setField(&form, transferLocalField, "./a")
	setField(&form, transferInstField, "/tmp/a")
	got, err := transferFrom(form, "web")
	if err != nil || got != (client.Transfer{Instance: "web", LocalPath: "./a", InstancePath: "/tmp/a", ToInstance: true}) {
		t.Fatalf("to instance: %+v %v", got, err)
	}
	toggle(&form, transferFromField)
	if got, _ = transferFrom(form, "web"); got.ToInstance {
		t.Error("expected the toggle to copy out of the instance")
	}
}

func TestTransferSubmit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	m := instancesModelWith(&anvilv1.Instance{Name: "nginx", State: anvilv1.State_STATE_RUNNING,
		Container: &anvilv1.ContainerSpec{ContainerId: "abc"}})
	m.client = &client.Client{}
	next, _ := m.Update(key("t"))
	m = next.(model)
	toggle(&m.instances.promptForm, transferFromField)
	setField(&m.instances.promptForm, transferLocalField, "out")
	setField(&m.instances.promptForm, transferInstField, "/etc/nginx")
	next, cmd := m.updateInstancesPrompt(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if cmd == nil || m.status != "" || !m.instances.lastTransferFrom {
		t.Fatalf("expected the copy to run: status %q", m.status)
	}

	next, _ = m.Update(transferDoneMsg{what: "nginx:/etc/nginx → out"})
	if s := next.(model).status; !strings.Contains(s, "copied nginx:/etc/nginx") {
		t.Errorf("status %q", s)
	}
}
