package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingBackend counts lifecycle calls and implements Resizer and Snapshotter.
type recordingBackend struct {
	plainBackend
	mu       sync.Mutex
	starts   []string
	startErr error
	resized  map[string]int64
	snaps    map[string][]Snapshot
	hook     func(string, State)
}

func (b *recordingBackend) Start(_ context.Context, s *Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts = append(b.starts, s.Name)
	return b.startErr
}

func (b *recordingBackend) SetExitHook(hook func(string, State)) { b.hook = hook }

func (b *recordingBackend) ResizeDisk(_ context.Context, s *Spec, gib int64) (string, error) {
	if b.resized == nil {
		b.resized = map[string]int64{}
	}
	b.resized[s.Name] = gib
	return fmt.Sprintf("disk grown to %d GiB", gib), nil
}

func (b *recordingBackend) CreateSnapshot(_ context.Context, s *Spec, name string) error {
	if b.snaps == nil {
		b.snaps = map[string][]Snapshot{}
	}
	b.snaps[s.Name] = append(b.snaps[s.Name], Snapshot{Name: name, CreatedAt: time.Now()})
	return nil
}
func (b *recordingBackend) RestoreSnapshot(context.Context, *Spec, string) error { return nil }
func (b *recordingBackend) DeleteSnapshot(_ context.Context, s *Spec, name string) error {
	var kept []Snapshot
	for _, sn := range b.snaps[s.Name] {
		if sn.Name != name {
			kept = append(kept, sn)
		}
	}
	b.snaps[s.Name] = kept
	return nil
}
func (b *recordingBackend) ListSnapshots(_ context.Context, s *Spec) ([]Snapshot, error) {
	return b.snaps[s.Name], nil
}

func (b *recordingBackend) startCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.starts)
}

func TestParseRestartPolicy(t *testing.T) {
	for in, want := range map[string]RestartPolicy{
		"":             {Mode: RestartNo},
		"no":           {Mode: RestartNo},
		"always":       {Mode: RestartAlways},
		"on-failure":   {Mode: RestartOnFailure},
		"on-failure:3": {Mode: RestartOnFailure, MaxRetries: 3},
	} {
		got, err := ParseRestartPolicy(in)
		if err != nil || got != want {
			t.Errorf("ParseRestartPolicy(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if in != "" && got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	for _, bad := range []string{"sometimes", "always:2", "on-failure:x", "on-failure:-1"} {
		if _, err := ParseRestartPolicy(bad); err == nil {
			t.Errorf("expected an error for %q", bad)
		}
	}
}

func TestRestartDelayBacksOff(t *testing.T) {
	if restartDelay(0) != time.Second || restartDelay(3) != 8*time.Second || restartDelay(20) != restartBackoffMax {
		t.Errorf("unexpected delays: %s %s %s", restartDelay(0), restartDelay(3), restartDelay(20))
	}
}

func newPolicyManager(t *testing.T, specs ...*Spec) (*Manager, *recordingBackend, *memRegistry) {
	t.Helper()
	rb := &recordingBackend{}
	reg := &memRegistry{}
	for _, s := range specs {
		_ = reg.PutInstance(s)
	}
	m := NewManager(reg, map[Kind]Backend{KindVM: rb})
	t.Cleanup(func() {
		for _, s := range specs {
			m.restarts.cancel(s.ID)
		}
	})
	return m, rb, reg
}

func pendingRestart(m *Manager, id string) bool {
	m.restarts.mu.Lock()
	defer m.restarts.mu.Unlock()
	return m.restarts.timers[id] != nil
}

func TestOnFailureRestartsCrashedInstance(t *testing.T) {
	m, rb, reg := newPolicyManager(t, &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning,
		RestartPolicy: RestartPolicy{Mode: RestartOnFailure}})

	rb.hook("01", StateStopped) // a clean exit doesn't count as a failure
	if pendingRestart(m, "01") {
		t.Fatal("on-failure must not restart after a clean exit")
	}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning, RestartPolicy: RestartPolicy{Mode: RestartOnFailure}})
	rb.hook("01", StateError)
	if !pendingRestart(m, "01") {
		t.Fatal("expected a pending restart after a crash")
	}
	m.restartNow("01")
	if rb.startCount() != 1 {
		t.Fatalf("expected one start, got %v", rb.starts)
	}
	if spec, _ := reg.GetByID("01"); spec.State != StateRunning {
		t.Errorf("expected the instance running again, got %s", spec.State)
	}
}

func TestAlwaysRestartsAfterPoweroffButNotAfterStop(t *testing.T) {
	m, rb, _ := newPolicyManager(t, &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning,
		RestartPolicy: RestartPolicy{Mode: RestartAlways}})
	rb.hook("01", StateStopped)
	if !pendingRestart(m, "01") {
		t.Fatal("always must restart after a guest poweroff")
	}

	// An explicit stop cancels it, and the next exit report is ignored anyway.
	if err := m.Stop(context.Background(), []string{"web"}, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if pendingRestart(m, "01") {
		t.Fatal("expected Stop to cancel the pending restart")
	}
	m.restartNow("01")
	if rb.startCount() != 0 {
		t.Errorf("expected no restart of a stopped-on-purpose instance, got %v", rb.starts)
	}
}

func TestOnFailureGivesUpAfterMaxRetries(t *testing.T) {
	spec := &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning, RestartPolicy: RestartPolicy{Mode: RestartOnFailure, MaxRetries: 2}}
	m, rb, reg := newPolicyManager(t, spec)
	rb.startErr = errors.New("boom")
	rb.hook("01", StateError)
	for i := 0; i < 5; i++ {
		if !pendingRestart(m, "01") {
			break
		}
		m.restartNow("01")
	}
	if rb.startCount() != 2 {
		t.Errorf("expected exactly 2 restart attempts, got %d", rb.startCount())
	}
	if got, _ := reg.GetByID("01"); got.State != StateError {
		t.Errorf("expected the instance left errored, got %s", got.State)
	}
}

func TestAutostart(t *testing.T) {
	m, rb, _ := newPolicyManager(t,
		&Spec{ID: "01", Name: "auto", Kind: KindVM, State: StateStopped, Autostart: true},
		&Spec{ID: "02", Name: "crashed", Kind: KindVM, State: StateError, Autostart: true},
		&Spec{ID: "03", Name: "stopped-on-purpose", Kind: KindVM, State: StateStopped, Autostart: true, UserStopped: true},
		&Spec{ID: "04", Name: "manual", Kind: KindVM, State: StateStopped},
		&Spec{ID: "05", Name: "deleted", Kind: KindVM, State: StateDeleted, Autostart: true},
	)
	m.Autostart(context.Background())
	got := map[string]bool{}
	for _, n := range rb.starts {
		got[n] = true
	}
	if len(got) != 2 || !got["auto"] || !got["crashed"] {
		t.Errorf("expected auto and crashed to start, got %v", rb.starts)
	}
}

func TestStartAndStopTrackUserStopped(t *testing.T) {
	m, _, reg := newPolicyManager(t, &Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning})
	_ = m.Stop(context.Background(), []string{"web"}, false, time.Second)
	if s, _ := reg.GetByID("01"); !s.UserStopped {
		t.Error("expected Stop to set UserStopped")
	}
	_ = m.Start(context.Background(), []string{"web"})
	if s, _ := reg.GetByID("01"); s.UserStopped {
		t.Error("expected Start to clear UserStopped")
	}
}

func intp(n int) *int     { return &n }
func i64p(n int64) *int64 { return &n }
func boolp(b bool) *bool  { return &b }

func TestUpdate(t *testing.T) {
	m, rb, reg := newPolicyManager(t,
		&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning, VM: &VMSpec{CPUs: 1, MemoryMiB: 1024}},
		&Spec{ID: "02", Name: "nginx", Kind: KindContainer, State: StateRunning, Container: &ContainerSpec{}},
	)
	always := RestartPolicy{Mode: RestartAlways}
	res, err := m.Update(context.Background(), "web", UpdateParams{
		CPUs: intp(4), MemoryMiB: i64p(4096), DiskGiB: i64p(40), Autostart: boolp(true), RestartPolicy: &always,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RestartPending {
		t.Error("expected cpu/memory changes on a running VM to wait for a restart")
	}
	if rb.resized["web"] != 40 {
		t.Errorf("expected the backend to grow the disk to 40, got %v", rb.resized)
	}
	s, _ := reg.GetByID("01")
	if s.VM.CPUs != 4 || s.VM.MemoryMiB != 4096 || s.VM.DiskGiB != 40 || !s.Autostart || s.RestartPolicy != always {
		t.Errorf("unexpected record after Update: %+v %+v", s, s.VM)
	}
	if len(res.Notes) < 5 {
		t.Errorf("expected a note per change, got %v", res.Notes)
	}

	if _, err := m.Update(context.Background(), "web", UpdateParams{CPUs: intp(0)}); err == nil {
		t.Error("expected an error for 0 cpus")
	}
	if _, err := m.Update(context.Background(), "nginx", UpdateParams{MemoryMiB: i64p(512)}); err == nil {
		t.Error("expected resources to be VM-only")
	}
	res, err = m.Update(context.Background(), "nginx", UpdateParams{Autostart: boolp(true)})
	if err != nil || res.RestartPending {
		t.Errorf("expected autostart to apply to a container right away, got %+v, %v", res, err)
	}
}

// liveBackend adds a scripted LiveResizer to recordingBackend.
type liveBackend struct {
	recordingBackend
	cpuErr error
}

func (b *liveBackend) SetCPUsLive(_ context.Context, _ *Spec, n int) (string, error) {
	if b.cpuErr != nil {
		return "", b.cpuErr
	}
	return fmt.Sprintf("cpus set to %d live", n), nil
}

func (b *liveBackend) SetMemoryLive(_ context.Context, _ *Spec, mib int64) (string, error) {
	return "", fmt.Errorf("guest said no: %w", ErrNeedsRestart)
}

func TestUpdateAppliesLiveWhenPossible(t *testing.T) {
	lb := &liveBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning, VM: &VMSpec{CPUs: 1, MemoryMiB: 1024}})
	m := NewManager(reg, map[Kind]Backend{KindVM: lb})

	res, err := m.Update(context.Background(), "web", UpdateParams{CPUs: intp(2)})
	if err != nil || res.RestartPending || res.Notes[0] != "cpus set to 2 live" {
		t.Errorf("expected a live cpu change, got %+v, %v", res, err)
	}
	res, err = m.Update(context.Background(), "web", UpdateParams{MemoryMiB: i64p(4096)})
	if err != nil || !res.RestartPending || !strings.Contains(res.Notes[0], "guest said no") {
		t.Errorf("expected a pending memory change with the reason, got %+v, %v", res, err)
	}
	if s, _ := reg.GetByID("01"); s.VM.CPUs != 2 || s.VM.MemoryMiB != 4096 {
		t.Errorf("expected both values recorded, got %+v", s.VM)
	}

	lb.cpuErr = errors.New("qmp broke")
	if _, err := m.Update(context.Background(), "web", UpdateParams{CPUs: intp(3)}); err == nil {
		t.Error("expected a real failure to be returned")
	}
	if s, _ := reg.GetByID("01"); s.VM.CPUs != 2 {
		t.Errorf("expected a failed change not to be recorded, got %d", s.VM.CPUs)
	}
}
