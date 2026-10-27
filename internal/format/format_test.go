package format

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestInstanceIP(t *testing.T) {
	bridged := &anvilv1.Instance{
		Vm:    &anvilv1.VMSpec{StaticIp: "10.55.1.4/24"},
		Guest: &anvilv1.GuestInfo{IpAddresses: []string{"10.55.1.4/24", "172.17.0.1/16"}},
	}
	if got := InstanceIP(bridged); got != "10.55.1.4" {
		t.Errorf("bridged VM: got %q", got)
	}
	slirp := &anvilv1.Instance{Vm: &anvilv1.VMSpec{}, Guest: &anvilv1.GuestInfo{IpAddresses: []string{"10.0.2.15/24"}}}
	if got := InstanceIP(slirp); got != "10.0.2.15" {
		t.Errorf("SLIRP VM with agent: got %q", got)
	}
	if got := InstanceIP(&anvilv1.Instance{Container: &anvilv1.ContainerSpec{}}); got != "" {
		t.Errorf("no address: got %q", got)
	}
}

func TestPolicyLabels(t *testing.T) {
	if got := RestartPolicy(nil); got != "no" {
		t.Errorf("nil policy: got %q", got)
	}
	if got := RestartPolicy(&anvilv1.RestartPolicy{Mode: "on-failure", MaxRetries: 3}); got != "on-failure:3" {
		t.Errorf("got %q", got)
	}
	if got := Schedule(&anvilv1.SnapshotSchedule{EverySeconds: 6 * 3600, Keep: 8}, "01-02"); got != "every 6h0m0s, keep 8" {
		t.Errorf("got %q", got)
	}
	if got := Schedule(nil, "01-02"); got != "" {
		t.Errorf("no schedule: got %q", got)
	}
}

func TestEnumLabels(t *testing.T) {
	cases := []struct{ got, want string }{
		{Kind(anvilv1.Kind_KIND_VM), "vm"},
		{Kind(anvilv1.Kind_KIND_CONTAINER), "container"},
		{Engine(0), "docker"},
		{Engine(anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN), "podman"},
		{Hypervisor(0), "qemu"},
		{Hypervisor(anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER), "firecracker"},
		{State(anvilv1.State_STATE_RUNNING), "running"},
		{State(0), "unknown"},
		{CloudInit(anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_DONE), "done"},
		{ShortImageID("sha256:0123456789abcdef"), "0123456789ab"},
		{ShortImageID("abc"), "abc"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestUptime(t *testing.T) {
	cases := []struct {
		secs int64
		sep  string
		want string
	}{
		{0, "", "-"},
		{42, "", "42s"},
		{125, " ", "2m 5s"},
		{3*3600 + 60, "", "3h1m"},
		{26*3600 + 120, " ", "1d 2h 2m"},
	}
	for _, c := range cases {
		if got := Uptime(c.secs, c.sep); got != c.want {
			t.Errorf("Uptime(%d, %q) = %q, want %q", c.secs, c.sep, got, c.want)
		}
	}
}
