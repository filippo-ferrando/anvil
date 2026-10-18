package daemon

import (
	"context"
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
