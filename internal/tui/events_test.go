package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestEventsFilter(t *testing.T) {
	m := model{screen: screenEvents, events: newEventsModel(), instances: newInstancesModel()}
	m.instances.list.SetItems([]list.Item{instanceItem{inst: &anvilv1.Instance{Name: "web"}}})

	next, _ := m.Update(key("f"))
	m = next.(model)
	if !m.events.filtering {
		t.Fatal("expected f to open the filter form")
	}
	if got := m.events.filterForm.fields[0].Suggestions; len(got) != 1 || got[0] != "web" {
		t.Errorf("instance suggestions = %q", got)
	}

	setField(&m.events.filterForm, "Limit", "-1")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if !m.events.filtering || m.events.filterForm.errMsg == "" {
		t.Fatal("expected a negative limit to be refused")
	}

	setField(&m.events.filterForm, "Instance", "web")
	setField(&m.events.filterForm, "Limit", "20")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if m.events.filtering || cmd == nil {
		t.Fatal("expected the form to close and reload")
	}
	if m.events.name != "web" || m.events.limit != 20 || m.events.list.Title != "Events of web" {
		t.Errorf("filter = %q/%d, title %q", m.events.name, m.events.limit, m.events.list.Title)
	}
}
