package instance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memHistory is an in-memory HistoryStore.
type memHistory struct {
	mu     sync.Mutex
	events []HistoryEvent
}

func (h *memHistory) AppendEvent(ev HistoryEvent) (HistoryEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev.Seq = uint64(len(h.events) + 1)
	h.events = append(h.events, ev)
	return ev, nil
}

func (h *memHistory) ListEvents(name string, _ int) ([]HistoryEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []HistoryEvent
	for _, ev := range h.events {
		if name == "" || ev.InstanceName == name {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (h *memHistory) last(t *testing.T) HistoryEvent {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.events) == 0 {
		t.Fatal("no event recorded")
	}
	return h.events[len(h.events)-1]
}

func TestOperationsRecordEventsWithActor(t *testing.T) {
	m, rb, _ := newPolicyManager(t, &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateStopped})
	h := &memHistory{}
	m.History = h
	ctx := WithActor(context.Background(), "alice")

	if err := m.Start(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if ev := h.last(t); ev.Action != "start" || ev.Actor != "alice" || ev.InstanceID != "01" || ev.Err != "" {
		t.Errorf("unexpected start event %+v", ev)
	}

	rb.startErr = errors.New("no qemu")
	_ = m.Start(ctx, []string{"web"})
	if ev := h.last(t); ev.Action != "start" || ev.Err == "" {
		t.Errorf("expected a failed start event, got %+v", ev)
	}

	if err := m.Stop(ctx, []string{"web"}, true, time.Second); err != nil {
		t.Fatal(err)
	}
	if ev := h.last(t); ev.Action != "stop" || ev.Message != "forced" {
		t.Errorf("unexpected stop event %+v", ev)
	}

	if err := m.Delete(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if ev := h.last(t); ev.Action != "delete" || ev.InstanceName != "web" {
		t.Errorf("unexpected delete event %+v", ev)
	}
}

func TestOutsideExitRecordsReasonAsDaemon(t *testing.T) {
	m, rb, _ := newPolicyManager(t, &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	h := &memHistory{}
	m.History = h

	rb.hook("01", StateError, "QEMU exited on its own (signal: killed)")
	ev := h.last(t)
	if ev.Action != "exit" || ev.Actor != DaemonActor || ev.Message != "QEMU exited on its own (signal: killed)" {
		t.Errorf("unexpected exit event %+v", ev)
	}
}

func TestFailedLaunchRecordsNoInstanceID(t *testing.T) {
	m, rb, _ := newPolicyManager(t)
	h := &memHistory{}
	m.History = h
	rb.startErr = errors.New("boom")

	_ = m.Launch(context.Background(), LaunchParams{Name: "db", Kind: KindVM, VM: &VMSpec{ImageRef: "debian-12"}}, func(LaunchEvent) {})
	ev := h.last(t)
	if ev.Action != "launch" || ev.InstanceID != "" || ev.InstanceName != "db" || ev.Err != "boom" {
		t.Errorf("unexpected launch event %+v", ev)
	}
}

func TestSubscribersSeeRecordedEvents(t *testing.T) {
	m, _, _ := newPolicyManager(t, &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateStopped})
	live, cancel := m.SubscribeEvents()
	defer cancel()

	_ = m.Start(context.Background(), []string{"web"})
	select {
	case ev := <-live:
		if ev.Action != "start" || ev.Actor != DaemonActor {
			t.Errorf("unexpected live event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no live event")
	}
}

// statsBackend reports fixed stats and stops the instance after a set number of samples.
type statsBackend struct {
	plainBackend
	reg       *memRegistry
	stopAfter int
	samples   int
}

func (b *statsBackend) Stats(_ context.Context, spec *Spec) (Stats, error) {
	b.samples++
	if b.samples == b.stopAfter {
		spec.State = StateStopped
		_ = b.reg.PutInstance(spec)
	}
	return Stats{CPUPercent: 42}, nil
}

func TestStreamStatsEndsCleanlyWhenStopped(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	sb := &statsBackend{reg: reg, stopAfter: 3}
	m := NewManager(reg, map[Kind]Backend{KindVM: sb})

	var sent int
	err := m.StreamStats(context.Background(), "web", time.Millisecond, func(Stats) error {
		sent++
		return nil
	})
	if err != nil {
		t.Fatalf("expected a clean end, got %v", err)
	}
	if sent != 2 {
		t.Errorf("expected the 2 samples taken while running, got %d", sent)
	}
}
