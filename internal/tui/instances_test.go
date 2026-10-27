package tui

import (
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
	req, err := buildStopRequest("web", form)
	if err != nil || req.GetForce() || req.GetTimeoutSeconds() != 30 {
		t.Fatalf("defaults: got %+v, %v", req, err)
	}
	toggle(&form, "Force")
	setField(&form, "Timeout seconds", "5")
	if req, err = buildStopRequest("web", form); err != nil || !req.GetForce() || req.GetTimeoutSeconds() != 5 || req.GetNames()[0] != "web" {
		t.Errorf("got %+v, %v", req, err)
	}
	setField(&form, "Timeout seconds", "x")
	if _, err := buildStopRequest("web", form); err == nil {
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
