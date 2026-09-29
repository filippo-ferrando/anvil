package daemon

import (
	"context"
	"testing"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func TestUpdateRPC(t *testing.T) {
	s := newTestServer(t, readyBackend{})
	stream := &recordingStream{}
	if err := s.Launch(&anvilv1.LaunchRequest{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{ImageRef: "x", Cpus: 1},
		RestartPolicy: &anvilv1.RestartPolicy{Mode: "on-failure:2"}}, stream); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(context.Background(), &anvilv1.InfoRequest{Names: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := info.GetInstances()[0].GetRestartPolicy(); p.GetMode() != "on-failure" || p.GetMaxRetries() != 2 {
		t.Errorf("expected the launch policy to be kept, got %+v", p)
	}

	cpus, auto := int32(4), true
	reply, err := s.Update(context.Background(), &anvilv1.UpdateRequest{Name: "web", Cpus: &cpus, Autostart: &auto,
		RestartPolicy: &anvilv1.RestartPolicy{Mode: "always"}})
	if err != nil {
		t.Fatal(err)
	}
	inst := reply.GetInstance()
	if inst.GetVm().GetCpus() != 4 || !inst.GetAutostart() || inst.GetRestartPolicy().GetMode() != "always" {
		t.Errorf("unexpected instance after Update: %+v", inst)
	}
	if !reply.GetRestartPending() {
		t.Error("expected a cpu change on a running VM to be pending")
	}

	if _, err := s.Update(context.Background(), &anvilv1.UpdateRequest{Name: "web", RestartPolicy: &anvilv1.RestartPolicy{Mode: "sometimes"}}); err == nil {
		t.Error("expected an unknown restart mode to be refused")
	}
	disk := int64(40)
	if _, err := s.Update(context.Background(), &anvilv1.UpdateRequest{Name: "web", DiskGib: &disk}); err == nil {
		t.Error("expected a backend without Resizer to refuse a disk change")
	}
}
