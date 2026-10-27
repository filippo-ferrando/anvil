package intent

import (
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

func TestHostsFor(t *testing.T) {
	members := []store.IntentMember{
		{Role: "db", Kind: instance.KindVM, IP: "10.0.0.2"},
		{Role: "cache", Kind: instance.KindContainer, IP: "10.0.0.3"},
		{Role: "web", Kind: instance.KindVM},
	}
	if got := hostsFor(members, false); len(got) != 2 || got["db"] != "10.0.0.2" || got["cache"] != "10.0.0.3" {
		t.Errorf("all members: %v", got)
	}
	if got := hostsFor(members, true); len(got) != 1 || got["db"] != "10.0.0.2" {
		t.Errorf("VMs only: %v", got)
	}
	if got := hostsFor(members[2:], false); got != nil {
		t.Errorf("no addresses: %v, want nil", got)
	}
}
