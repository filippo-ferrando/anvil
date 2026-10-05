package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func TestDiscoveredHostPrefillsAddForm(t *testing.T) {
	m := model{screen: screenMigration, migration: newMigrationModel()}

	next, _ := m.Update(hostsDiscoveredMsg{hosts: []*anvilv1.DiscoveredHost{
		{Name: "forge", Address: "192.168.1.9", SshPort: 2222},
	}})
	m = next.(model)
	if !m.migration.browsing {
		t.Fatal("expected the discovered list to show after a scan")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if !m.migration.adding {
		t.Fatal("expected enter on a discovered host to open the add form")
	}
	if got := m.migration.addForm.Value("Alias"); got != "forge" {
		t.Errorf("alias: got %q, want %q", got, "forge")
	}
	// The port is only carried when it isn't SSH's own.
	if got := m.migration.addForm.Value("user@host[:port]"); got != "root@192.168.1.9:2222" {
		t.Errorf("target: got %q, want %q", got, "root@192.168.1.9:2222")
	}
}

func TestDiscoveredAddressOmitsDefaultSSHPort(t *testing.T) {
	if got := discoveredAddress(&anvilv1.DiscoveredHost{Address: "10.0.0.2", SshPort: 22}); got != "10.0.0.2" {
		t.Errorf("got %q, want %q", got, "10.0.0.2")
	}
}
