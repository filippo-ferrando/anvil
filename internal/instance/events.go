package instance

import (
	"log"
	"sync"
)

// EventType says what happened to an instance record.
type EventType int

const (
	EventUpdated EventType = iota + 1 // created or changed (state, config, ...)
	EventDeleted                      // record removed from the registry
)

// Event is one change to an instance record. Spec is a fresh copy read back from
// the registry; for EventDeleted only its ID and Name are reliable.
type Event struct {
	Type EventType
	Spec *Spec
}

// subscriberBuffer is how many events a slow subscriber may lag behind before
// newer ones are dropped for it.
const subscriberBuffer = 256

type broadcaster struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

func (b *broadcaster) subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = make(map[int]chan Event)
	}
	id := b.next
	b.next++
	ch := make(chan Event, subscriberBuffer)
	b.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}

func (b *broadcaster) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			log.Printf("instance: dropping %v event for %s, a watcher is too slow", ev.Type, ev.Spec.Name)
		}
	}
}

// notifyingRegistry publishes an Event after every successful write.
type notifyingRegistry struct {
	Registry
	events   *broadcaster
	decorate func(*Spec) *Spec // adds live fields (guest info) before publishing
}

func (r notifyingRegistry) PutInstance(spec *Spec) error {
	if err := r.Registry.PutInstance(spec); err != nil {
		return err
	}
	// Read the record back so subscribers never share memory with the caller's spec.
	if fresh, err := r.Registry.GetByID(spec.ID); err == nil {
		if r.decorate != nil {
			fresh = r.decorate(fresh)
		}
		r.events.publish(Event{Type: EventUpdated, Spec: fresh})
	}
	return nil
}

func (r notifyingRegistry) DeleteByID(id string) error {
	gone := &Spec{ID: id}
	if old, err := r.Registry.GetByID(id); err == nil {
		gone = &Spec{ID: old.ID, Name: old.Name, Kind: old.Kind}
	}
	if err := r.Registry.DeleteByID(id); err != nil {
		return err
	}
	r.events.publish(Event{Type: EventDeleted, Spec: gone})
	return nil
}

// Subscribe returns a channel of instance changes and a func that ends the
// subscription. Events are change hints: a lagging subscriber may miss some.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	return m.events.subscribe()
}
