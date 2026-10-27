package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// eventsStream hands each sent event to a channel.
type eventsStream struct {
	anvilv1.InstanceService_EventsServer
	ctx  context.Context
	sent chan *anvilv1.InstanceEvent
}

func (s *eventsStream) Send(ev *anvilv1.InstanceEvent) error {
	s.sent <- ev
	return nil
}

func (s *eventsStream) Context() context.Context { return s.ctx }

func TestEventsFollowSendsStoredThenLiveForOneInstance(t *testing.T) {
	s := newTestServer(t, readyBackend{})
	launch := func(name string) {
		req := &anvilv1.LaunchRequest{Name: name, Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{ImageRef: "x"}}
		if err := s.Launch(req, &recordingStream{}); err != nil {
			t.Fatal(err)
		}
	}
	launch("web")

	ctx, cancel := context.WithCancel(context.Background())
	stream := &eventsStream{ctx: ctx, sent: make(chan *anvilv1.InstanceEvent, 10)}
	done := make(chan error, 1)
	go func() { done <- s.Events(&anvilv1.EventsRequest{Name: "web", Follow: true}, stream) }()

	next := func() *anvilv1.InstanceEvent {
		select {
		case ev := <-stream.sent:
			return ev
		case <-time.After(2 * time.Second):
			t.Fatal("no event sent")
			return nil
		}
	}
	if ev := next(); ev.GetAction() != "launch" || ev.GetSeq() != 1 {
		t.Fatalf("expected the stored launch first, got %v", ev)
	}

	launch("db") // filtered out: another instance
	if err := s.Manager.Stop(instance.WithActor(ctx, "alice"), []string{"web"}, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.GetAction() != "stop" || ev.GetActor() != "alice" || ev.GetInstanceName() != "web" {
		t.Fatalf("expected web's live stop, got %v", ev)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Events: %v", err)
	}
	if len(stream.sent) != 0 {
		t.Errorf("unexpected extra event %v", <-stream.sent)
	}
}

// A uid's name is looked up once, then served from the cache.
func TestUIDNamesCaches(t *testing.T) {
	calls := 0
	c := &uidNames{lookup: func(uid string) (string, error) {
		calls++
		if uid == "1000" {
			return "alice", nil
		}
		return "", errors.New("unknown")
	}}
	for range 3 {
		if got := c.name(1000); got != "alice" {
			t.Fatalf("name = %q", got)
		}
	}
	if got := c.name(42); got != "uid 42" {
		t.Errorf("unknown uid = %q", got)
	}
	if calls != 2 {
		t.Errorf("lookup ran %d times, want 2", calls)
	}
}
