package instance

import (
	"sync"
	"testing"
	"time"
)

// memRegistry is an in-memory Registry that stores copies, like the bbolt one does.
type memRegistry struct {
	mu    sync.Mutex
	specs map[string]Spec
}

func (r *memRegistry) PutInstance(spec *Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.specs == nil {
		r.specs = map[string]Spec{}
	}
	r.specs[spec.ID] = *spec
	return nil
}

func (r *memRegistry) GetByID(id string) (*Spec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.specs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &s, nil
}

func (r *memRegistry) GetByName(name string) (*Spec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.specs {
		if s.Name == name {
			return &s, nil
		}
	}
	return nil, ErrNotFound
}

func (r *memRegistry) List(Kind) ([]*Spec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Spec
	for _, s := range r.specs {
		out = append(out, &s)
	}
	return out, nil
}

func (r *memRegistry) DeleteByID(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.specs, id)
	return nil
}

func nextEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

func TestManagerPublishesRegistryChanges(t *testing.T) {
	m := NewManager(&memRegistry{}, nil)
	events, cancel := m.Subscribe()
	defer cancel()

	spec := &Spec{ID: "01", Name: "web", State: StateRunning}
	if err := m.registry.PutInstance(spec); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, events)
	if ev.Type != EventUpdated || ev.Spec.Name != "web" || ev.Spec.State != StateRunning {
		t.Errorf("unexpected event: %+v", ev)
	}
	if ev.Spec == spec {
		t.Error("expected the event to carry its own copy of the spec")
	}

	if err := m.registry.DeleteByID("01"); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, events)
	if ev.Type != EventDeleted || ev.Spec.ID != "01" || ev.Spec.Name != "web" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestSubscribeCancelStopsDelivery(t *testing.T) {
	m := NewManager(&memRegistry{}, nil)
	events, cancel := m.Subscribe()
	cancel()
	cancel() // safe to call twice
	if _, open := <-events; open {
		t.Error("expected the channel to be closed after cancel")
	}
	// Publishing with no subscribers must not block or panic.
	if err := m.registry.PutInstance(&Spec{ID: "02", Name: "db"}); err != nil {
		t.Fatal(err)
	}
}

func TestSlowSubscriberDoesNotBlockWrites(t *testing.T) {
	m := NewManager(&memRegistry{}, nil)
	_, cancel := m.Subscribe() // never read
	defer cancel()
	done := make(chan struct{})
	go func() {
		for range subscriberBuffer + 10 {
			_ = m.registry.PutInstance(&Spec{ID: "03", Name: "cache"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writes blocked on a subscriber that never reads")
	}
}
