package daemon

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// readyBackend is a do-nothing VM backend whose WaitReady result is scripted.
type readyBackend struct {
	readyErr error
}

func (readyBackend) Create(context.Context, *instance.Spec, func(string)) error      { return nil }
func (readyBackend) Start(context.Context, *instance.Spec) error                     { return nil }
func (readyBackend) Stop(context.Context, *instance.Spec, bool, time.Duration) error { return nil }
func (readyBackend) Delete(context.Context, *instance.Spec) error                    { return nil }
func (readyBackend) Status(context.Context, *instance.Spec) (instance.State, error) {
	return instance.StateRunning, nil
}
func (readyBackend) Logs(context.Context, *instance.Spec, bool, int, func([]byte) error) error {
	return io.EOF
}
func (b readyBackend) WaitReady(_ context.Context, _ *instance.Spec, progress func(string)) error {
	progress("waiting for cloud-init: running")
	return b.readyErr
}

// recordingStream captures what a server-streaming handler sends.
type recordingStream struct {
	anvilv1.InstanceService_LaunchServer
	events []*anvilv1.LaunchProgress
}

func (s *recordingStream) Send(ev *anvilv1.LaunchProgress) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *recordingStream) Context() context.Context { return context.Background() }

func newTestServer(t *testing.T, b instance.Backend) *Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServer(instance.NewManager(db, map[instance.Kind]instance.Backend{instance.KindVM: b}), nil)
}

func TestLaunchWaitHoldsInstanceEventUntilReady(t *testing.T) {
	s := newTestServer(t, readyBackend{})
	stream := &recordingStream{}
	req := &anvilv1.LaunchRequest{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{ImageRef: "x"}, Wait: true}
	if err := s.Launch(req, stream); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	last := stream.events[len(stream.events)-1]
	if last.GetInstance().GetName() != "web" {
		t.Fatalf("expected the stream to end with the instance, got %v", last)
	}
	waitIdx, instIdx := -1, -1
	for i, ev := range stream.events {
		if ev.GetStatus() == "waiting for cloud-init: running" {
			waitIdx = i
		}
		if ev.GetInstance() != nil {
			instIdx = i
		}
	}
	if waitIdx == -1 || instIdx < waitIdx {
		t.Errorf("expected the wait progress before the only Instance event, got %v", stream.events)
	}
}

func TestLaunchWaitReportsCloudInitFailure(t *testing.T) {
	s := newTestServer(t, readyBackend{readyErr: errors.New("cloud-init finished with errors")})
	stream := &recordingStream{}
	req := &anvilv1.LaunchRequest{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{ImageRef: "x"}, Wait: true}
	if err := s.Launch(req, stream); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	last := stream.events[len(stream.events)-1]
	if last.GetError() == "" {
		t.Fatalf("expected the stream to end with an error, got %v", last)
	}
	for _, ev := range stream.events {
		if ev.GetInstance() != nil {
			t.Error("expected no Instance event when waiting failed")
		}
	}
}

func TestLaunchWithoutWaitSendsInstanceRightAway(t *testing.T) {
	s := newTestServer(t, readyBackend{readyErr: errors.New("must not be called")})
	stream := &recordingStream{}
	req := &anvilv1.LaunchRequest{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{ImageRef: "x"}}
	if err := s.Launch(req, stream); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if stream.events[len(stream.events)-1].GetInstance() == nil {
		t.Errorf("expected the stream to end with the instance, got %v", stream.events)
	}
}
