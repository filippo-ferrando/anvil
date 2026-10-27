package tui

import (
	"slices"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
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

func TestMigrateFormSuggestions(t *testing.T) {
	m := model{screen: screenMigration, migration: newMigrationModel()}
	for _, msg := range []tea.Msg{
		instancesLoadedMsg{instances: []*anvilv1.Instance{{Name: "parrot"}, {Name: "web"}}},
		intentsLoadedMsg{intents: []*anvilv1.Intent{{Name: "lab"}}},
		hostsLoadedMsg{hosts: []*anvilv1.Host{{Alias: "forge"}, {Alias: "nas"}}},
	} {
		next, _ := m.Update(msg)
		m = next.(model)
	}

	form := m.migration.migrateForm
	if got := filterSuggestions(form.fields[0].Suggestions, ""); !slices.Equal(got, []string{"parrot", "web", "lab"}) {
		t.Errorf("Name suggestions: got %q", got)
	}
	if got := form.fields[1].Suggestions; !slices.Equal(got, []string{"forge", "nas"}) {
		t.Errorf("To suggestions: got %q", got)
	}

	// Tab on the Name field fills in the first match.
	m.migration.focusForm = true
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("par")})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if got := m.migration.migrateForm.Value("Name"); got != "parrot" {
		t.Errorf("tab completion: got %q, want %q", got, "parrot")
	}
}

func TestBuildMigrateRequest(t *testing.T) {
	form := newMigrateForm()
	if _, err := buildMigrateRequest(form); err == nil {
		t.Error("expected an error without Name and To")
	}
	setField(&form, "Name", "web")
	setField(&form, "To", "forge")
	setField(&form, destNameField, "web-2")
	toggle(&form, "Copy")
	req, err := buildMigrateRequest(form)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetDestName() != "web-2" || !req.GetCopy() || req.GetDryRun() {
		t.Errorf("unexpected request %+v", req)
	}
}

func TestDiscoverTimeoutForm(t *testing.T) {
	m := model{screen: screenMigration, migration: newMigrationModel()}
	next, _ := m.Update(key("D"))
	m = next.(model)
	if !m.migration.discovering {
		t.Fatal("expected D to open the discover form")
	}
	setField(&m.migration.discoverForm, "Timeout", "later")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if !m.migration.discovering || m.migration.discoverForm.errMsg == "" {
		t.Error("expected a bad timeout to keep the form open with an error")
	}
}
