package tui

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func cloudInitTestModel() model {
	m := tasksTestModel()
	m.screen = screenCloudInit
	m.cloudInit = newCloudInitModel()
	m.cloudInit.current, m.cloudInit.dirty = "a", true
	m.cloudInit.editor.SetValue("content of a")
	return m
}

func TestCloudInitSavedOnlyAppliesToItsDoc(t *testing.T) {
	m := cloudInitTestModel()
	// A new config (n, m, w) is loaded, not renamed onto the doc in the editor.
	next, cmd := m.updateCloudInit(cloudInitSavedMsg{name: "b"})
	if got := next.(model).cloudInit; got.current != "a" || cmd == nil {
		t.Fatalf("new config: current %q, want a until b's content loads", got.current)
	}
	// A save of a doc the user already left changes nothing in the editor.
	next, _ = m.updateCloudInit(cloudInitSavedMsg{name: "z", from: "z"})
	if got := next.(model).cloudInit; got.current != "a" || !got.dirty {
		t.Fatalf("stale save: current %q, dirty %v", got.current, got.dirty)
	}
	// ctrl+s and rename of the doc in the editor keep its content.
	next, _ = m.updateCloudInit(cloudInitSavedMsg{name: "c", from: "a"})
	if got := next.(model).cloudInit; got.current != "c" || got.dirty || got.editor.Value() != "content of a" {
		t.Fatalf("rename: current %q, dirty %v", got.current, got.dirty)
	}
}

func TestCloudInitDeleteClearsOnlyItsDoc(t *testing.T) {
	m := cloudInitTestModel()
	next, _ := m.updateCloudInit(actionDoneMsg{screen: screenCloudInit, verb: "deleted", name: "other"})
	if got := next.(model).cloudInit; got.current != "a" || got.editor.Value() != "content of a" {
		t.Fatalf("deleting another config cleared the editor: %q", got.current)
	}
	next, _ = m.updateCloudInit(actionDoneMsg{screen: screenCloudInit, verb: "deleted", name: "a"})
	if got := next.(model).cloudInit; got.current != "" || got.dirty || got.editor.Value() != "" {
		t.Fatalf("deleting the open config: current %q, dirty %v", got.current, got.dirty)
	}
}

func TestManifestDeleteClearsOnlyItsDoc(t *testing.T) {
	m := model{screen: screenManifests, manifests: newManifestsModel()}
	m.manifests.current, m.manifests.dirty = "a", true
	m.manifests.editor.SetValue("content of a")
	next, _ := m.updateManifests(actionDoneMsg{screen: screenManifests, verb: "deleted", name: "other"})
	if got := next.(model).manifests; got.current != "a" || got.editor.Value() != "content of a" {
		t.Fatalf("deleting another manifest cleared the editor: %q", got.current)
	}
	next, _ = m.updateManifests(actionDoneMsg{screen: screenManifests, verb: "deleted", name: "a"})
	if got := next.(model).manifests; got.current != "" || got.dirty {
		t.Fatalf("deleting the open manifest: current %q, dirty %v", got.current, got.dirty)
	}
}

func TestCloudInitImportsKeepTheirOwnTask(t *testing.T) {
	m := cloudInitTestModel()
	a := m.startTask("import a", "imported a", screenCloudInit)
	b := m.startTask("import b", "imported b", screenCloudInit)
	m.cloudInit.importTask = b.id
	step := func(task int, s streamStep[*anvilv1.CloudInitImportResult]) {
		t.Helper()
		next, _ := m.updateCloudInit(cloudInitImportStreamMsg{s, task})
		m = next.(model)
	}
	step(a.id, streamStep[*anvilv1.CloudInitImportResult]{result: &anvilv1.CloudInitImportResult{Name: "from-a"}})
	step(a.id, streamStep[*anvilv1.CloudInitImportResult]{done: true})
	if a.running || !b.running {
		t.Fatalf("a running %v, b running %v; want only a finished", a.running, b.running)
	}
	if len(b.steps.lines) != 0 || len(m.cloudInit.importLines) != 0 {
		t.Fatalf("a's events reached b: %q, %q", b.steps.lines, m.cloudInit.importLines)
	}
	step(b.id, streamStep[*anvilv1.CloudInitImportResult]{done: true})
	if b.running {
		t.Fatal("b should finish on its own end")
	}
}
