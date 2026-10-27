package instance

import (
	"context"
	"testing"
)

// exitBackend is a Backend that only records the exit hook it was given.
type exitBackend struct {
	plainBackend
	hook   func(string, State, string)
	status State // what Status reports
}

func (b *exitBackend) Status(context.Context, *Spec) (State, error) { return b.status, nil }

func (b *exitBackend) SetExitHook(hook func(string, State, string)) { b.hook = hook }

func TestRecordExitUpdatesRunningInstance(t *testing.T) {
	eb := &exitBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	_ = reg.PutInstance(&Spec{ID: "02", Name: "db", Kind: KindVM, State: StateStopped})
	m := NewManager(reg, map[Kind]Backend{KindVM: eb})
	if eb.hook == nil {
		t.Fatal("expected NewManager to install the exit hook")
	}
	events, cancel := m.Subscribe()
	defer cancel()

	eb.hook("01", StateError, "test")
	if spec, _ := reg.GetByID("01"); spec.State != StateError {
		t.Errorf("expected web to be recorded as errored, got %s", spec.State)
	}
	if ev := nextEvent(t, events); ev.Spec.ID != "01" || ev.Spec.State != StateError {
		t.Errorf("expected a Watch event for web, got %+v", ev)
	}

	// An instance someone already stopped keeps its state.
	eb.hook("02", StateError, "test")
	if spec, _ := reg.GetByID("02"); spec.State != StateStopped {
		t.Errorf("expected db to stay stopped, got %s", spec.State)
	}
	eb.hook("missing", StateStopped, "test") // must not panic
}

func TestRecordExitStartOutsideAnvil(t *testing.T) {
	eb := &exitBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindContainer, State: StateError})
	_ = reg.PutInstance(&Spec{ID: "02", Name: "gone", Kind: KindContainer, State: StateDeleting})
	NewManager(reg, map[Kind]Backend{KindContainer: eb})

	eb.hook("01", StateRunning, "test")
	if spec, _ := reg.GetByID("01"); spec.State != StateRunning {
		t.Errorf("expected an outside restart to mark web running, got %s", spec.State)
	}
	eb.hook("02", StateRunning, "test")
	if spec, _ := reg.GetByID("02"); spec.State != StateDeleting {
		t.Errorf("expected a record being deleted to stay deleting, got %s", spec.State)
	}
}

func TestRecordExitIgnoresStaleReport(t *testing.T) {
	eb := &exitBackend{status: StateRunning}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	NewManager(reg, map[Kind]Backend{KindVM: eb})

	// The old process exited, but a start already brought up a new one.
	eb.hook("01", StateStopped, "test")
	if spec, _ := reg.GetByID("01"); spec.State != StateRunning {
		t.Errorf("expected a stale exit report to be ignored, got %s", spec.State)
	}
}
