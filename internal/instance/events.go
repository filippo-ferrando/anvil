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

// broadcaster fans out values of T to every subscriber.
type broadcaster[T any] struct {
	mu   sync.Mutex
	subs map[int]chan T
	next int
}

func (b *broadcaster[T]) subscribe() (<-chan T, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = make(map[int]chan T)
	}
	id := b.next
	b.next++
	ch := make(chan T, subscriberBuffer)
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

func (b *broadcaster[T]) publish(ev T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			log.Printf("instance: dropping an event, a watcher is too slow")
		}
	}
}

// notifyingRegistry publishes an Event after every successful write.
type notifyingRegistry struct {
	Registry
	events   *broadcaster[Event]
	decorate func(*Spec) *Spec // adds live fields (guest info) before publishing
}

func (r notifyingRegistry) PutInstance(spec *Spec) error {
	if err := r.Registry.PutInstance(spec); err != nil {
		return err
	}
	r.publishUpdated(spec.ID)
	return nil
}

func (r notifyingRegistry) UpdateInstance(id string, fn func(*Spec) error) error {
	if err := r.Registry.UpdateInstance(id, fn); err != nil {
		return err
	}
	r.publishUpdated(id)
	return nil
}

// publishUpdated reads the record back, so subscribers never share memory with a caller's spec.
func (r notifyingRegistry) publishUpdated(id string) {
	if fresh, err := r.Registry.GetByID(id); err == nil {
		if r.decorate != nil {
			fresh = r.decorate(fresh)
		}
		r.events.publish(Event{Type: EventUpdated, Spec: fresh})
	}
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
