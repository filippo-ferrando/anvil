package client

import (
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestDiscoveredAddress(t *testing.T) {
	if got := DiscoveredAddress(&anvilv1.DiscoveredHost{Address: "10.0.0.2", SshPort: 22}); got != "10.0.0.2" {
		t.Errorf("default port: got %q", got)
	}
	if got := DiscoveredAddress(&anvilv1.DiscoveredHost{Address: "10.0.0.2", SshPort: 2222}); got != "10.0.0.2:2222" {
		t.Errorf("other port: got %q", got)
	}
}

func TestFindDiscovered(t *testing.T) {
	hosts := []*anvilv1.DiscoveredHost{
		{Name: "alpha", Address: "10.0.0.2"},
		{Name: "beta", Address: "10.0.0.3"},
		{Name: "beta", Address: "10.0.0.4"},
	}
	if h, err := FindDiscovered(hosts, "alpha"); err != nil || h.GetAddress() != "10.0.0.2" {
		t.Errorf("by name: %v, %v", h, err)
	}
	if h, err := FindDiscovered(hosts, "10.0.0.4"); err != nil || h.GetName() != "beta" {
		t.Errorf("by address: %v, %v", h, err)
	}
	if _, err := FindDiscovered(hosts, "beta"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := FindDiscovered(hosts, "gamma"); err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("missing: %v", err)
	}
	if _, err := FindDiscovered(nil, "gamma"); err == nil || !strings.Contains(err.Error(), "no anvil hosts") {
		t.Errorf("none: %v", err)
	}
}
