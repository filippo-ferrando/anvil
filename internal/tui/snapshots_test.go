package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// Moving the VM cursor waits for it to settle before listing snapshots.
func TestSnapshotsSelectionSettles(t *testing.T) {
	m := model{screen: screenSnapshots, snapshots: newSnapshotsModel()}
	m.snapshots.setSize(80, 20)
	m.snapshots.instances.SetItems([]list.Item{
		instanceItem{inst: &anvilv1.Instance{Name: "a", Kind: anvilv1.Kind_KIND_VM}},
		instanceItem{inst: &anvilv1.Instance{Name: "b", Kind: anvilv1.Kind_KIND_VM}},
	})

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if !m.snapshots.loading || cmd == nil {
		t.Fatal("expected a settle tick after moving the cursor")
	}
	// A tick for a row the cursor already left does nothing.
	if _, cmd := m.Update(snapshotsSettledMsg{name: "a"}); cmd != nil {
		t.Error("a stale settle tick must not list snapshots")
	}
	if _, cmd := m.Update(snapshotsSettledMsg{name: "b"}); cmd == nil {
		t.Error("expected the settled row's snapshots to load")
	}
}
