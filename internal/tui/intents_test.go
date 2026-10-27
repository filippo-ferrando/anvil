package tui

import (
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestIntentInfoShowsNetworkAndIDs(t *testing.T) {
	m := newIntentsModel()
	m.showingInfo = &anvilv1.Intent{
		Name: "lab",
		Network: &anvilv1.IntentNetwork{
			EngineNetworkName: "anvil-lab", BridgeInterface: "br-lab", Subnet: "10.55.1.0/24", Gateway: "10.55.1.1",
		},
		Members: []*anvilv1.IntentMember{{InstanceId: "abc123", Role: "db", Kind: anvilv1.Kind_KIND_VM}},
	}
	view := m.View()
	for _, want := range []string{"anvil-lab", "br-lab", "10.55.1.0/24", "abc123"} {
		if !strings.Contains(view, want) {
			t.Errorf("expected %q in the info view:\n%s", want, view)
		}
	}
}
