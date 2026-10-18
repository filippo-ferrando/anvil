package instance

import (
	"context"
	"log"
	"time"
)

// HistoryEvent is one entry of the event log behind `anvil events`.
type HistoryEvent struct {
	Seq          uint64 // set by the HistoryStore, grows by one per event
	Time         time.Time
	InstanceID   string // empty when the operation failed before an instance existed
	InstanceName string
	Kind         Kind
	Action       string
	Actor        string // the caller's user name, or DaemonActor
	Message      string
	Err          string // set when the operation failed
}

// HistoryStore persists HistoryEvents, keeping only the newest ones.
type HistoryStore interface {
	// AppendEvent stores ev and returns it with Seq set.
	AppendEvent(ev HistoryEvent) (HistoryEvent, error)
	// ListEvents returns the newest limit events of instanceName (all instances when empty), oldest first.
	// limit 0 returns every stored match.
	ListEvents(instanceName string, limit int) ([]HistoryEvent, error)
}

// DaemonActor is the actor of events anvild causes on its own (exits, restart policy, schedules).
const DaemonActor = "anvild"

type actorKey struct{}

// WithActor tags ctx with the user name that the operations run under it are recorded for.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorFrom(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return DaemonActor
}

// record logs action on spec, with err's text when it failed. Storage errors are only logged:
// a broken event log never fails the operation itself.
func (m *Manager) record(ctx context.Context, spec *Spec, action, message string, err error) {
	ev := HistoryEvent{
		Time:         time.Now(),
		InstanceID:   spec.ID,
		InstanceName: spec.Name,
		Kind:         spec.Kind,
		Action:       action,
		Actor:        actorFrom(ctx),
		Message:      message,
	}
	if err != nil {
		ev.Err = err.Error()
	}
	if m.History != nil {
		stored, sErr := m.History.AppendEvent(ev)
		if sErr != nil {
			log.Printf("instance: recording %s event for %s: %v", action, spec.Name, sErr)
		} else {
			ev = stored
		}
	}
	m.history.publish(ev)
}

// ListEvents returns the newest limit stored events of name (every instance when empty), oldest first.
func (m *Manager) ListEvents(name string, limit int) ([]HistoryEvent, error) {
	if m.History == nil {
		return nil, nil
	}
	return m.History.ListEvents(name, limit)
}

// SubscribeEvents returns a channel of new events and a func that ends the subscription.
func (m *Manager) SubscribeEvents() (<-chan HistoryEvent, func()) {
	return m.history.subscribe()
}
