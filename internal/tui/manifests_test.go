package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func manifestsKey(t *testing.T, m model, k string) model {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return next.(model)
}

func TestManifestsGuards(t *testing.T) {
	m := model{screen: screenManifests, manifests: newManifestsModel()}

	if m = manifestsKey(t, m, "a"); m.manifests.prompt != manifestPromptNone || !m.statusBad {
		t.Fatal("apply with nothing loaded should only report an error")
	}

	next, _ := m.Update(manifestContentLoadedMsg{name: "shop", content: "members: {}\n"})
	m = next.(model)
	if m = manifestsKey(t, m, "x"); m.manifests.prompt != manifestPromptNone {
		t.Fatal("teardown without an intent: field should not prompt")
	}

	m.manifests.intent, m.manifests.dirty = "shop", true
	if m = manifestsKey(t, m, "a"); m.manifests.prompt != manifestPromptNone {
		t.Fatal("apply with unsaved edits should not prompt")
	}
	if m = manifestsKey(t, m, "x"); m.manifests.prompt != manifestPromptTeardown {
		t.Fatal("teardown with an intent should ask for confirmation")
	}
}

// TestApplyStreamGoesToOwner checks an apply started on one page never lands in the other's transcript.
func TestApplyStreamGoesToOwner(t *testing.T) {
	m := model{screen: screenIntents, intents: newIntentsModel(), manifests: newManifestsModel()}
	m.manifests.apply.req = &anvilv1.IntentApplyRequest{DryRun: true}
	m.manifests.apply.running = true

	next, _ := m.Update(applyStreamMsg{owner: screenManifests, streamStep: streamStep[[]string]{result: []string{"plan"}, done: true}})
	m = next.(model)
	if len(m.manifests.apply.lines) != 1 || !m.manifests.apply.confirm {
		t.Fatalf("manifests apply = %+v, want the plan waiting for confirmation", m.manifests.apply)
	}
	if m.intents.apply.active() {
		t.Fatal("the intents page picked up a manifests apply")
	}
}
