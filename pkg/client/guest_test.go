package client

import (
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestSSHTarget(t *testing.T) {
	running := anvilv1.State_STATE_RUNNING
	slirp := &anvilv1.Instance{Id: "id1", Name: "a", State: running,
		Vm: &anvilv1.VMSpec{SshPort: 2222, DefaultUser: "ubuntu"}}
	bridged := &anvilv1.Instance{Id: "id2", Name: "b", State: running,
		Vm: &anvilv1.VMSpec{NetworkMode: "bridge", StaticIp: "10.0.0.5/24"}}

	local := &Client{}
	got, err := local.SSHTarget(slirp, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "localhost" || got.Port != 2222 || got.User != "ubuntu" || got.Jump != nil || got.InstanceID != "id1" {
		t.Fatalf("slirp: %+v", got)
	}

	remote := &Client{Remote: "me@box"}
	got, err = remote.SSHTarget(bridged, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "10.0.0.5" || got.Port != 22 || got.User != "admin" || len(got.Jump) != 2 {
		t.Fatalf("bridged: %+v", got)
	}
	if got, _ := local.SSHTarget(bridged, ""); got.User != "root" {
		t.Fatalf("default user: %q", got.User)
	}

	bad := []*anvilv1.Instance{
		{Name: "ctr", State: running, Container: &anvilv1.ContainerSpec{}},
		{Name: "off", State: anvilv1.State_STATE_STOPPED, Vm: &anvilv1.VMSpec{SshPort: 1}},
		{Name: "noport", State: running, Vm: &anvilv1.VMSpec{}},
		{Name: "noip", State: running, Vm: &anvilv1.VMSpec{NetworkMode: "bridge"}},
	}
	for _, inst := range bad {
		if _, err := local.SSHTarget(inst, ""); err == nil || !strings.Contains(err.Error(), inst.GetName()) {
			t.Fatalf("%s: want an error naming it, got %v", inst.GetName(), err)
		}
	}
}

func TestSSHArgs(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	args, err := SSHArgs(SSHTarget{InstanceID: "id1", Port: 2222, Jump: []string{"-J", "me@box"}}, "-P", "/key")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-P 2222", "-J me@box", "-i /key", "known_hosts/id1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}
