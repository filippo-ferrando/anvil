package commands

import (
	"strings"
	"testing"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func TestFormatWatchEvent(t *testing.T) {
	at := time.Date(2026, 9, 29, 18, 4, 5, 0, time.UTC)
	updated := formatWatchEvent(at, &anvilv1.WatchEvent{
		Type:     anvilv1.WatchEventType_WATCH_EVENT_TYPE_UPDATED,
		Instance: &anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, State: anvilv1.State_STATE_RUNNING},
	})
	for _, want := range []string{"18:04:05", "web", "Running"} {
		if !strings.Contains(updated, want) {
			t.Errorf("expected %q in %q", want, updated)
		}
	}
	deleted := formatWatchEvent(at, &anvilv1.WatchEvent{
		Type:     anvilv1.WatchEventType_WATCH_EVENT_TYPE_DELETED,
		Instance: &anvilv1.Instance{Name: "db"},
	})
	if !strings.Contains(deleted, "db") || !strings.Contains(deleted, "Removed") {
		t.Errorf("unexpected deleted line %q", deleted)
	}
}

func TestInstanceIP(t *testing.T) {
	bridged := &anvilv1.Instance{
		Vm:    &anvilv1.VMSpec{StaticIp: "10.55.1.4/24"},
		Guest: &anvilv1.GuestInfo{IpAddresses: []string{"10.55.1.4/24", "172.17.0.1/16"}},
	}
	if got := instanceIP(bridged); got != "10.55.1.4" {
		t.Errorf("bridged VM: got %q", got)
	}
	slirp := &anvilv1.Instance{Vm: &anvilv1.VMSpec{}, Guest: &anvilv1.GuestInfo{IpAddresses: []string{"10.0.2.15/24"}}}
	if got := instanceIP(slirp); got != "10.0.2.15" {
		t.Errorf("SLIRP VM with agent: got %q", got)
	}
	if got := instanceIP(&anvilv1.Instance{Container: &anvilv1.ContainerSpec{}}); got != "-" {
		t.Errorf("no address: got %q", got)
	}
}

func TestPrintGuestInfo(t *testing.T) {
	var buf strings.Builder
	printGuestInfo(&buf, &anvilv1.Instance{
		State: anvilv1.State_STATE_RUNNING,
		Vm:    &anvilv1.VMSpec{},
		Guest: &anvilv1.GuestInfo{AgentConnected: true, CloudInit: anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_DONE, IpAddresses: []string{"10.0.2.15/24"}},
	})
	for _, want := range []string{"Guest agent:\tconnected", "Cloud-init:\tdone", "IP:\t10.0.2.15/24"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("expected %q in:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	printGuestInfo(&buf, &anvilv1.Instance{State: anvilv1.State_STATE_STOPPED, Vm: &anvilv1.VMSpec{}})
	if buf.Len() != 0 {
		t.Errorf("expected nothing for a stopped VM, got %q", buf.String())
	}
}
