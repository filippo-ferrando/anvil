package instance

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// guestBackend is a Backend with a scripted guest cache and ready result.
type guestBackend struct {
	mu       sync.Mutex
	info     map[string]GuestInfo
	hook     func(string)
	readyErr error
	waited   []string
}

func (b *guestBackend) Create(context.Context, *Spec, func(string)) error      { return nil }
func (b *guestBackend) Start(context.Context, *Spec) error                     { return nil }
func (b *guestBackend) Stop(context.Context, *Spec, bool, time.Duration) error { return nil }
func (b *guestBackend) Delete(context.Context, *Spec) error                    { return nil }
func (b *guestBackend) Status(context.Context, *Spec) (State, error)           { return StateRunning, nil }
func (b *guestBackend) Logs(context.Context, *Spec, bool, int, func([]byte) error) error {
	return io.EOF
}

func (b *guestBackend) GuestInfo(spec *Spec) (GuestInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	info, ok := b.info[spec.ID]
	return info, ok
}

func (b *guestBackend) SetGuestChangeHook(hook func(string)) { b.hook = hook }

func (b *guestBackend) WaitReady(ctx context.Context, spec *Spec, progress func(string)) error {
	b.waited = append(b.waited, spec.Name)
	progress("waiting")
	return b.readyErr
}

func TestManagerAddsGuestInfo(t *testing.T) {
	gb := &guestBackend{info: map[string]GuestInfo{"01": {AgentConnected: true, IPAddresses: []string{"10.0.2.15/24"}}}}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	_ = reg.PutInstance(&Spec{ID: "02", Name: "db", Kind: KindVM, State: StateStopped})
	m := NewManager(reg, map[Kind]Backend{KindVM: gb})

	specs, err := m.Info([]string{"web", "db"})
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Guest == nil || specs[0].Guest.IPAddresses[0] != "10.0.2.15/24" {
		t.Errorf("expected web to carry its guest info, got %+v", specs[0].Guest)
	}
	if specs[1].Guest != nil {
		t.Errorf("expected no guest info for db, got %+v", specs[1].Guest)
	}
}

func TestGuestChangeHookPublishesEvent(t *testing.T) {
	gb := &guestBackend{info: map[string]GuestInfo{}}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	m := NewManager(reg, map[Kind]Backend{KindVM: gb})
	if gb.hook == nil {
		t.Fatal("expected NewManager to install the guest change hook")
	}
	events, cancel := m.Subscribe()
	defer cancel()

	gb.mu.Lock()
	gb.info["01"] = GuestInfo{AgentConnected: true, CloudInit: CloudInitDone}
	gb.mu.Unlock()
	gb.hook("01")

	ev := nextEvent(t, events)
	if ev.Type != EventUpdated || ev.Spec.Guest == nil || ev.Spec.Guest.CloudInit != CloudInitDone {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestManagerWaitReady(t *testing.T) {
	gb := &guestBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	_ = reg.PutInstance(&Spec{ID: "02", Name: "off", Kind: KindVM, State: StateStopped})
	_ = reg.PutInstance(&Spec{ID: "03", Name: "nginx", Kind: KindContainer, State: StateRunning})
	m := NewManager(reg, map[Kind]Backend{KindVM: gb, KindContainer: &plainBackend{}})

	var statuses []string
	if err := m.WaitReady(context.Background(), "web", func(s string) { statuses = append(statuses, s) }); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if len(gb.waited) != 1 || len(statuses) != 1 {
		t.Errorf("expected one backend wait with progress, got waited=%v statuses=%v", gb.waited, statuses)
	}

	if err := m.WaitReady(context.Background(), "off", nil); err == nil {
		t.Error("expected an error for a stopped instance")
	}
	if err := m.WaitReady(context.Background(), "nginx", nil); err != nil {
		t.Errorf("expected a backend without ReadyWaiter to be ready right away, got %v", err)
	}

	gb.readyErr = errors.New("cloud-init failed")
	if err := m.WaitReady(context.Background(), "web", func(string) {}); err == nil {
		t.Error("expected the backend's error to come through")
	}
}

// plainBackend implements only Backend, none of the optional interfaces.
type plainBackend struct{}

func (plainBackend) Create(context.Context, *Spec, func(string)) error                { return nil }
func (plainBackend) Start(context.Context, *Spec) error                               { return nil }
func (plainBackend) Stop(context.Context, *Spec, bool, time.Duration) error           { return nil }
func (plainBackend) Delete(context.Context, *Spec) error                              { return nil }
func (plainBackend) Status(context.Context, *Spec) (State, error)                     { return StateRunning, nil }
func (plainBackend) Logs(context.Context, *Spec, bool, int, func([]byte) error) error { return io.EOF }
