package instance

import "testing"

// exitBackend is a Backend that only records the exit hook it was given.
type exitBackend struct {
	plainBackend
	hook func(string, State)
}

func (b *exitBackend) SetExitHook(hook func(string, State)) { b.hook = hook }

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

	eb.hook("01", StateError)
	if spec, _ := reg.GetByID("01"); spec.State != StateError {
		t.Errorf("expected web to be recorded as errored, got %s", spec.State)
	}
	if ev := nextEvent(t, events); ev.Spec.ID != "01" || ev.Spec.State != StateError {
		t.Errorf("expected a Watch event for web, got %+v", ev)
	}

	// An instance someone already stopped keeps its state.
	eb.hook("02", StateError)
	if spec, _ := reg.GetByID("02"); spec.State != StateStopped {
		t.Errorf("expected db to stay stopped, got %s", spec.State)
	}
	eb.hook("missing", StateStopped) // must not panic
}

func TestRecordExitStartOutsideAnvil(t *testing.T) {
	eb := &exitBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindContainer, State: StateError})
	_ = reg.PutInstance(&Spec{ID: "02", Name: "gone", Kind: KindContainer, State: StateDeleted})
	NewManager(reg, map[Kind]Backend{KindContainer: eb})

	eb.hook("01", StateRunning)
	if spec, _ := reg.GetByID("01"); spec.State != StateRunning {
		t.Errorf("expected an outside restart to mark web running, got %s", spec.State)
	}
	eb.hook("02", StateRunning)
	if spec, _ := reg.GetByID("02"); spec.State != StateDeleted {
		t.Errorf("expected a deleted record to stay deleted, got %s", spec.State)
	}
}
