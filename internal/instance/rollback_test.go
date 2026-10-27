package instance

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// failingCreateBackend fails Create and counts Delete calls, with the ctx each one got.
type failingCreateBackend struct {
	plainBackend
	deletes   int
	deleteErr error
}

func (b *failingCreateBackend) Create(context.Context, *Spec, func(string)) error {
	return errors.New("boom")
}
func (b *failingCreateBackend) Delete(ctx context.Context, _ *Spec) error {
	b.deletes++
	b.deleteErr = ctx.Err()
	return nil
}

func TestLaunchRollsBackFailedCreateOnCancelledRequest(t *testing.T) {
	b := &failingCreateBackend{}
	m := NewManager(&memRegistry{}, map[Kind]Backend{KindVM: b})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client went away
	if err := m.Launch(ctx, LaunchParams{Name: "web", Kind: KindVM, VM: &VMSpec{}}, func(LaunchEvent) {}); err == nil {
		t.Fatal("expected the launch to fail")
	}
	if b.deletes != 1 || b.deleteErr != nil {
		t.Errorf("expected one rollback on a live ctx, got %d deletes, ctx err %v", b.deletes, b.deleteErr)
	}
}

func TestLaunchCatchesExitBeforeRecordExists(t *testing.T) {
	m, rb, reg := newPolicyManager(t)
	rb.status = StateStopped // exited right after Start, before the record was written
	if err := m.Launch(context.Background(), LaunchParams{Name: "web", Kind: KindVM, VM: &VMSpec{}}, func(LaunchEvent) {}); err != nil {
		t.Fatal(err)
	}
	spec, err := reg.GetByName("web")
	if err != nil {
		t.Fatal(err)
	}
	if spec.State != StateStopped {
		t.Errorf("state = %s, want stopped", spec.State)
	}
}

// portRecreateBackend fails AddPort after swapping in a new container ID, as a
// recreate that rolled back to a fresh container does.
type portRecreateBackend struct{ plainBackend }

func (portRecreateBackend) AddPort(_ context.Context, s *Spec, _ PortMapping) error {
	s.Container.ContainerID = "new"
	return errors.New("port is already allocated")
}
func (portRecreateBackend) RemovePort(context.Context, *Spec, int, string) error { return nil }

func TestAddPortSavesSpecOnFailure(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindContainer, Container: &ContainerSpec{ContainerID: "old"}})
	m := NewManager(reg, map[Kind]Backend{KindContainer: portRecreateBackend{}})
	if err := m.AddPort(context.Background(), "web", PortMapping{HostPort: 8080, GuestPort: 80}); err == nil {
		t.Fatal("expected the port add to fail")
	}
	if s, _ := reg.GetByName("web"); s.Container.ContainerID != "new" {
		t.Errorf("container ID = %q, want the replacement saved", s.Container.ContainerID)
	}
}

func TestLaunchFillsDefaultSizes(t *testing.T) {
	reg := &memRegistry{}
	m := NewManager(reg, map[Kind]Backend{KindVM: plainBackend{}})
	if err := m.Launch(context.Background(), LaunchParams{Name: "web", Kind: KindVM, VM: &VMSpec{}, NoStart: true}, func(LaunchEvent) {}); err != nil {
		t.Fatal(err)
	}
	s, _ := reg.GetByName("web")
	if s.VM.CPUs != DefaultCPUs || s.VM.MemoryMiB != DefaultMemoryMiB {
		t.Errorf("sizes = %d CPUs, %d MiB; want the defaults", s.VM.CPUs, s.VM.MemoryMiB)
	}
}

// reconcileBackend reports every instance as running when anvild starts.
type reconcileBackend struct{ plainBackend }

func (reconcileBackend) Reconcile(context.Context, *Spec) (State, error) { return StateRunning, nil }

func TestReconcileAdoptsInstanceRecordedStopped(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateStopped, VM: &VMSpec{}})
	m := NewManager(reg, map[Kind]Backend{KindVM: reconcileBackend{}})
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, _ := reg.GetByName("web"); s.State != StateRunning {
		t.Errorf("state = %s, want running: the process outlived a crash before its record was saved", s.State)
	}
}

func TestCapabilityMissing(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, VM: &VMSpec{}})
	m := NewManager(reg, map[Kind]Backend{KindVM: plainBackend{}})
	if _, err := m.ListSnapshots(context.Background(), "web"); err == nil || !strings.Contains(err.Error(), "don't support snapshots") {
		t.Errorf("snapshots on a plain backend: %v", err)
	}
	if err := m.Mount(context.Background(), "web", "/a", "/b", false); err == nil || !strings.Contains(err.Error(), "don't support mounts") {
		t.Errorf("mount on a plain backend: %v", err)
	}
	if _, err := m.Stats(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing instance: %v", err)
	}
}
