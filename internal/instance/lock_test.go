package instance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingBackend blocks Create, Stop and Mount until release is closed, after
// signalling on entered. It counts Start calls.
type blockingBackend struct {
	plainBackend
	entered chan string
	release chan struct{}
	starts  atomic.Int32
}

func newBlockingBackend() *blockingBackend {
	return &blockingBackend{entered: make(chan string, 4), release: make(chan struct{})}
}

func (b *blockingBackend) block(op string) {
	b.entered <- op
	<-b.release
}

func (b *blockingBackend) Create(context.Context, *Spec, func(string)) error {
	b.block("create")
	return nil
}

func (b *blockingBackend) Start(context.Context, *Spec) error {
	b.starts.Add(1)
	return nil
}

func (b *blockingBackend) Stop(context.Context, *Spec, bool, time.Duration) error {
	b.block("stop")
	return nil
}

func (b *blockingBackend) Mount(_ context.Context, s *Spec, host, guest string, ro bool) error {
	b.block("mount")
	s.VM.Mounts = append(s.VM.Mounts, Mount{HostPath: host, GuestPath: guest, ReadOnly: ro})
	return nil
}

func (b *blockingBackend) Umount(context.Context, *Spec, string) error { return nil }

func TestBusyInstanceFailsFast(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "box", Kind: KindVM, State: StateRunning, VM: &VMSpec{}})
	b := newBlockingBackend()
	m := NewManager(reg, map[Kind]Backend{KindVM: b})

	done := make(chan error)
	go func() { done <- m.Mount(context.Background(), "box", "/src", "/mnt", false) }()
	<-b.entered

	if err := m.Delete(context.Background(), []string{"box"}); !errors.Is(err, ErrBusy) {
		t.Errorf("expected delete during a mount to fail with ErrBusy, got %v", err)
	}
	if err := m.Start(context.Background(), []string{"box"}); !errors.Is(err, ErrBusy) {
		t.Errorf("expected start during a mount to fail with ErrBusy, got %v", err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := reg.GetByID("01")
	if err != nil || len(got.VM.Mounts) != 1 {
		t.Errorf("expected the mount to be saved, got %+v, %v", got, err)
	}
	if err := m.Delete(context.Background(), []string{"box"}); err != nil {
		t.Errorf("expected delete to work once the mount ended, got %v", err)
	}
}

func TestSaveDoesNotResurrectDeleted(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "box", Kind: KindVM, VM: &VMSpec{}})
	m := NewManager(reg, map[Kind]Backend{KindVM: plainBackend{}})
	spec, _ := reg.GetByID("01")
	_ = reg.DeleteByID("01")
	if err := m.save(spec, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if _, err := reg.GetByID("01"); err == nil {
		t.Error("save recreated a deleted record")
	}
}

func TestSameNameLaunchesOnlyOneWins(t *testing.T) {
	reg := &memRegistry{}
	b := newBlockingBackend()
	m := NewManager(reg, map[Kind]Backend{KindVM: b})

	launch := func() error {
		return m.Launch(context.Background(), LaunchParams{Name: "web", Kind: KindVM, VM: &VMSpec{}, NoStart: true}, func(LaunchEvent) {})
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); errs[0] = launch() }()
	<-b.entered // the first launch holds the name while it provisions
	errs[1] = launch()
	close(b.release)
	wg.Wait()

	if errs[0] != nil {
		t.Errorf("expected the first launch to succeed, got %v", errs[0])
	}
	if errs[1] == nil {
		t.Error("expected the second launch of the same name to fail")
	}
	if specs, _ := reg.List(""); len(specs) != 1 {
		t.Errorf("expected one instance, got %d", len(specs))
	}
}

func TestRestartWaitsForStopAndRespectsIt(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "box", Kind: KindVM, State: StateError,
		RestartPolicy: RestartPolicy{Mode: RestartAlways}, VM: &VMSpec{}})
	b := newBlockingBackend()
	m := NewManager(reg, map[Kind]Backend{KindVM: b})

	done := make(chan error)
	go func() { done <- m.Stop(context.Background(), []string{"box"}, false, time.Second) }()
	<-b.entered

	restarted := make(chan struct{})
	go func() { m.restartNow("01"); close(restarted) }()
	select {
	case <-restarted:
		t.Fatal("expected the restart to wait for the stop")
	case <-time.After(50 * time.Millisecond):
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-restarted
	if n := b.starts.Load(); n != 0 {
		t.Errorf("expected no restart after a user stop, got %d starts", n)
	}
	if got, _ := reg.GetByID("01"); got.State != StateStopped || !got.UserStopped {
		t.Errorf("expected stopped by the user, got %+v", got)
	}
}
