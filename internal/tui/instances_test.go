package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func TestStopPrompt(t *testing.T) {
	running := &anvilv1.Instance{Name: "web", State: anvilv1.State_STATE_RUNNING, Vm: &anvilv1.VMSpec{}}
	m := instancesModelWith(running)
	next, _ := m.Update(key("S"))
	m = next.(model)
	if m.instances.prompt != instancesPromptStop {
		t.Fatal("expected S to open the stop prompt")
	}

	form := m.instances.promptForm
	req, err := buildStopRequest([]string{"web"}, form)
	if err != nil || req.GetForce() || req.GetTimeoutSeconds() != 30 {
		t.Fatalf("defaults: got %+v, %v", req, err)
	}
	toggle(&form, "Force")
	setField(&form, "Timeout seconds", "5")
	if req, err = buildStopRequest([]string{"web"}, form); err != nil || !req.GetForce() || req.GetTimeoutSeconds() != 5 || req.GetNames()[0] != "web" {
		t.Errorf("got %+v, %v", req, err)
	}
	setField(&form, "Timeout seconds", "x")
	if _, err := buildStopRequest([]string{"web"}, form); err == nil {
		t.Error("expected a bad timeout to fail")
	}

	stopped := instancesModelWith(&anvilv1.Instance{Name: "db", Vm: &anvilv1.VMSpec{}})
	next, _ = stopped.Update(key("S"))
	if next.(model).instances.prompt != instancesPromptNone {
		t.Error("expected no stop prompt for a stopped instance")
	}
}

func TestShellPromptsAskIdentityForVMsOnly(t *testing.T) {
	vm := instancesModelWith(&anvilv1.Instance{Name: "web", Vm: &anvilv1.VMSpec{}})
	for _, k := range []string{"x", "e", "g"} {
		next, _ := vm.Update(key(k))
		if !hasField(next.(model).instances.promptForm, identityLabel) {
			t.Errorf("%s: expected an identity field for a VM", k)
		}
	}
	ct := instancesModelWith(&anvilv1.Instance{Name: "nginx", Kind: anvilv1.Kind_KIND_CONTAINER, Container: &anvilv1.ContainerSpec{}})
	for _, k := range []string{"e", "g"} {
		next, _ := ct.Update(key(k))
		if hasField(next.(model).instances.promptForm, identityLabel) {
			t.Errorf("%s: expected no identity field for a container", k)
		}
	}
}

func TestLogsTailPrompt(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Vm: &anvilv1.VMSpec{}})
	next, _ := m.Update(key("L"))
	m = next.(model)
	if m.instances.prompt != instancesPromptLogs || !hasField(m.instances.promptForm, logsTailField) {
		t.Fatal("expected L to open the tail prompt")
	}
}

func TestDetailShowsContainerAndSSHRows(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{
		Name: "app", Kind: anvilv1.Kind_KIND_CONTAINER, GuiEnabled: true,
		Container: &anvilv1.ContainerSpec{
			ContainerId: "0123456789abcdef", NetworkAlias: "app.lab",
			Volumes:    []*anvilv1.VolumeMount{{HostPath: "/srv/data", ContainerPath: "/data", ReadOnly: true}},
			ExtraHosts: map[string]string{"db": "10.55.1.2"},
		},
	})
	m.instances.setSize(200, 60)
	info := m.instances.detailInfo()
	for _, want := range []string{"0123456789ab", "app.lab", "/data ← /srv/data (ro)", "db → 10.55.1.2", "g to run an app"} {
		if !strings.Contains(info, want) {
			t.Errorf("expected %q in:\n%s", want, info)
		}
	}

	vm := instancesModelWith(&anvilv1.Instance{Name: "web", Vm: &anvilv1.VMSpec{SshPort: 2222, DefaultUser: "ubuntu"}})
	vm.instances.setSize(200, 60)
	info = vm.instances.detailInfo()
	if !strings.Contains(info, "ubuntu@localhost, port 2222") || !strings.Contains(info, "not available") {
		t.Errorf("expected the SSH row and no GUI support in:\n%s", info)
	}
}

func TestRemoteHostPathFields(t *testing.T) {
	local := model{}
	if f := local.exportPathField("web"); !f.PathComplete || f.input.Value() != "web.tar.zst" {
		t.Errorf("local export field: complete=%v value=%q", f.PathComplete, f.input.Value())
	}
	remote := model{client: &client.Client{Remote: "me@far"}}
	f := remote.exportPathField("web")
	if f.PathComplete || f.input.Value() != "" || !strings.Contains(f.Hint, "me@far") {
		t.Errorf("remote export field: complete=%v value=%q hint=%q", f.PathComplete, f.input.Value(), f.Hint)
	}
}

func TestMountCreateDirToggleLocalOnly(t *testing.T) {
	vm := &anvilv1.Instance{Name: "web", Vm: &anvilv1.VMSpec{}}
	next, _ := instancesModelWith(vm).Update(key("m"))
	if !hasField(next.(model).instances.promptForm, createDirField) {
		t.Error("expected the create-dir toggle for a local daemon")
	}
	remote := instancesModelWith(vm)
	remote.client = &client.Client{Remote: "me@far"}
	next, _ = remote.Update(key("m"))
	if hasField(next.(model).instances.promptForm, createDirField) {
		t.Error("expected no create-dir toggle with --remote")
	}
}

func TestMountCreateDirFailureSkipsMount(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A nil client would panic if Mount were reached.
	msg := mountInstance(nil, "web", filepath.Join(file, "sub"), "/mnt", false, true)().(actionDoneMsg)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "create-dir") {
		t.Errorf("expected a create-dir error, got %v", msg.err)
	}
}

func TestMultiSelect(t *testing.T) {
	a := &anvilv1.Instance{Id: "1", Name: "a", State: anvilv1.State_STATE_RUNNING, Vm: &anvilv1.VMSpec{}}
	b := &anvilv1.Instance{Id: "2", Name: "b", Vm: &anvilv1.VMSpec{}}
	c := &anvilv1.Instance{Id: "3", Name: "c", State: anvilv1.State_STATE_RUNNING, Vm: &anvilv1.VMSpec{}}
	m := instancesModelWith(a, b, c)
	if got := instanceNames(m.instances.targets()); len(got) != 1 || got[0] != "a" {
		t.Fatalf("no marks: expected the selected instance, got %v", got)
	}

	// space marks a and moves down; space again marks b.
	for range 2 {
		next, _ := m.Update(key(" "))
		m = next.(model)
	}
	if got := instanceNames(m.instances.targets()); strings.Join(got, ",") != "a,b" {
		t.Fatalf("expected a,b marked, got %v", got)
	}
	if title := m.instances.list.Items()[0].(instanceItem).Title(); !strings.HasPrefix(title, "✓") {
		t.Errorf("expected a visible marker, got %q", title)
	}

	next, _ := m.Update(key("d"))
	m = next.(model)
	if len(m.instances.confirmDelete) != 2 || !strings.Contains(m.instances.View(), "2 instances (a, b)") {
		t.Errorf("expected the delete confirmation to list both:\n%s", m.instances.View())
	}
	next, cmd := m.Update(key("n")) // any other key cancels
	m = next.(model)
	if cmd != nil || len(m.instances.marked) != 2 {
		t.Error("expected a cancelled delete to keep the marks")
	}

	next, _ = m.Update(key("S"))
	m = next.(model)
	if strings.Join(m.instances.stopNames, ",") != "a" {
		t.Errorf("expected S to stop only the running marked instance, got %v", m.instances.stopNames)
	}
	m.instances.prompt = instancesPromptNone

	// A reload drops marks of instances that went away.
	next, _ = m.Update(instancesLoadedMsg{instances: []*anvilv1.Instance{b, c}})
	m = next.(model)
	if len(m.instances.marked) != 1 || !m.instances.marked["2"] {
		t.Errorf("expected only b to stay marked, got %v", m.instances.marked)
	}

	// s with a mark acts on the marks and clears them.
	if _, cmd := m.Update(key("s")); cmd == nil {
		t.Error("expected s to start the marked instance")
	}
	next, _ = m.Update(key("s"))
	if len(next.(model).instances.marked) != 0 {
		t.Error("expected s to clear the marks")
	}

	m.instances.toggleMark()
	next, _ = m.Update(key("esc"))
	if m = next.(model); len(m.instances.marked) != 0 || m.sidebarFocused {
		t.Error("expected esc to clear the marks before leaving the screen")
	}
}
