package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestMirrorAddFormFollowsKind(t *testing.T) {
	m := model{screen: screenMirrors, mirrors: newMirrorsModel()}
	next, _ := m.Update(key("a"))
	m = next.(model)
	setField(&m.mirrors.addForm, "Name", "local")
	if hasField(m.mirrors.addForm, "Insecure") || !hasField(m.mirrors.addForm, "Manifest URL") {
		t.Fatal("expected a VM mirror form with a manifest URL and no Insecure toggle")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	m = next.(model)
	form := m.mirrors.addForm
	if hasField(form, "Manifest URL") || !hasField(form, "Registry") {
		t.Fatal("expected the container fields after ctrl+k")
	}
	if form.Value("Name") != "local" {
		t.Error("expected Name to survive the kind switch")
	}
	setField(&form, "Registry", "registry.lan:5000")
	toggle(&form, "Insecure")

	mirror, err := buildMirror("container", form)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.GetKind() != anvilv1.MirrorKind_MIRROR_KIND_CONTAINER || !mirror.GetInsecure() || mirror.GetRegistry() != "registry.lan:5000" {
		t.Errorf("unexpected mirror %+v", mirror)
	}
}

func hasField(f simpleForm, label string) bool {
	for _, field := range f.fields {
		if field.Label == label {
			return true
		}
	}
	return false
}
