package tui

import (
	"errors"
	"testing"
)

func TestWatchEventsCollapseIntoOneReload(t *testing.T) {
	m := model{screen: screenInstances}

	next, cmd := m.Update(watchEventMsg{})
	m = next.(model)
	if !m.watchReloadPending || cmd == nil {
		t.Fatal("expected the first event to schedule a reload")
	}

	// A second event while a reload is pending must not schedule another one.
	next, _ = m.Update(watchEventMsg{})
	m = next.(model)
	if !m.watchReloadPending {
		t.Fatal("expected the reload to still be pending")
	}

	next, cmd = m.Update(watchReloadMsg{})
	m = next.(model)
	if m.watchReloadPending {
		t.Error("expected the pending flag to clear once the reload runs")
	}
	if cmd == nil {
		t.Error("expected a reload command on the Instances screen")
	}
}

func TestWatchReloadSkippedOnUnrelatedScreen(t *testing.T) {
	m := model{screen: screenMirrors, watchReloadPending: true}
	next, cmd := m.Update(watchReloadMsg{})
	if next.(model).watchReloadPending {
		t.Error("expected the pending flag to clear")
	}
	if cmd != nil {
		t.Error("expected no reload on a screen that doesn't show instances")
	}
}

func TestWatchErrorSchedulesRetry(t *testing.T) {
	m := model{screen: screenInstances}
	_, cmd := m.Update(watchEventMsg{err: errors.New("stream closed")})
	if cmd == nil {
		t.Error("expected a retry to be scheduled after the stream broke")
	}
}
