//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

type fakeProc struct {
	exited  chan struct{}
	stopped []time.Duration
}

func (p *fakeProc) Exited() <-chan struct{} { return p.exited }

func (p *fakeProc) Stop(ctx context.Context, timeout time.Duration) error {
	p.stopped = append(p.stopped, timeout)
	return nil
}

func TestVMTableStartGuard(t *testing.T) {
	tab := newVMTable[*fakeProc]()
	running, done, err := tab.beginStart("a")
	if running || err != nil {
		t.Fatalf("first beginStart = %v, %v", running, err)
	}
	if _, _, err := tab.beginStart("a"); err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("second beginStart = %v, want a concurrent call error", err)
	}
	if _, ok, starting := tab.beginStop("a"); ok || !starting {
		t.Fatalf("beginStop during Start = %v, %v", ok, starting)
	}
	done()
	p := &fakeProc{}
	tab.track("a", p)
	if running, _, err := tab.beginStart("a"); !running || err != nil {
		t.Fatalf("beginStart on a running VM = %v, %v", running, err)
	}
	if got, ok := tab.lookup("a"); !ok || got != p || !tab.tracked("a") {
		t.Fatalf("lookup = %v, %v", got, ok)
	}
}

func TestVMTableOwnExit(t *testing.T) {
	tab := newVMTable[*fakeProc]()
	var hooked string
	tab.SetExitHook(func(id string, _ instance.State, _ string) { hooked = id })
	p, newer := &fakeProc{}, &fakeProc{}
	tab.track("a", p)

	hook, own := tab.ownExit("a", p)
	if !own || hook == nil {
		t.Fatalf("ownExit of the current process = %v", own)
	}
	hook("a", instance.StateStopped, "")
	if hooked != "a" {
		t.Fatal("the hook returned isn't the one set")
	}
	if _, own := tab.ownExit("a", newer); own {
		t.Error("an exit of a replaced process was reported")
	}
	if got, ok, _ := tab.beginStop("a"); !ok || got != p {
		t.Fatal("beginStop missed the running process")
	}
	if _, own := tab.ownExit("a", p); own {
		t.Error("an exit during Stop was reported")
	}
	tab.endStop("a")
	if _, own := tab.ownExit("a", p); !own {
		t.Error("an exit after Stop ended was not reported")
	}
}

func TestGracefulStopWithoutAgent(t *testing.T) {
	g := newGuestTracker(func(string) *qemu.GuestAgent { return nil }, "powerdown")
	spec := &instance.Spec{ID: "a"}
	p := &fakeProc{exited: make(chan struct{})}
	if err := g.gracefulStop(t.Context(), spec, p, true, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := g.gracefulStop(t.Context(), spec, p, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := g.gracefulStop(t.Context(), spec, p, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(p.stopped) != 3 || p.stopped[0] != 0 || p.stopped[1] < 59*time.Second || p.stopped[2] > time.Second {
		t.Fatalf("Stop timeouts = %v, want 0, about 1m, then at most 1s", p.stopped)
	}
}

type fakeLifecycle struct {
	running        bool
	stopErr, start error
	calls          []string
}

func (f *fakeLifecycle) Start(context.Context, *instance.Spec) error {
	f.calls = append(f.calls, "start")
	return f.start
}

func (f *fakeLifecycle) Stop(context.Context, *instance.Spec, bool, time.Duration) error {
	f.calls = append(f.calls, "stop")
	return f.stopErr
}

func (f *fakeLifecycle) tracked(string) bool { return f.running }

func TestRestoreSnapshotRestartsARunningVM(t *testing.T) {
	spec := &instance.Spec{ID: "a", Name: "a"}
	apply := func(f *fakeLifecycle) func() error {
		return func() error { f.calls = append(f.calls, "apply"); return nil }
	}

	f := &fakeLifecycle{running: true}
	if err := restoreSnapshot(t.Context(), f, spec, "s", apply(f)); err != nil || strings.Join(f.calls, ",") != "stop,apply,start" {
		t.Fatalf("running VM: %v, calls %v", err, f.calls)
	}
	f = &fakeLifecycle{}
	if err := restoreSnapshot(t.Context(), f, spec, "s", apply(f)); err != nil || strings.Join(f.calls, ",") != "apply" {
		t.Fatalf("stopped VM: %v, calls %v", err, f.calls)
	}
	f = &fakeLifecycle{running: true, start: errors.New("boom")}
	err := restoreSnapshot(t.Context(), f, spec, "s", func() error { return errors.New("bad") })
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("both failing = %v, want both errors", err)
	}
	f = &fakeLifecycle{running: true, stopErr: errors.New("stuck")}
	if err := restoreSnapshot(t.Context(), f, spec, "s", apply(f)); err == nil || len(f.calls) != 1 {
		t.Fatalf("failed stop = %v, calls %v; want no apply", err, f.calls)
	}
}

func TestCheckNewMount(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &instance.Spec{Name: "a", VM: &instance.VMSpec{Mounts: []instance.Mount{{GuestPath: "/mnt/a"}}}}
	for _, c := range []struct {
		host, guest string
		limit       int
		want        string
	}{
		{dir, "/mnt/b", 0, ""},
		{dir, "/mnt/a", 0, "already has a mount"},
		{dir, "/mnt/b", 1, "maximum of 1"},
		{filepath.Join(dir, "missing"), "/mnt/b", 0, "host path"},
		{file, "/mnt/b", 0, "not a directory"},
	} {
		err := checkNewMount(spec, c.host, c.guest, c.limit)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("checkNewMount(%s, %s, %d) = %v, want %q", c.host, c.guest, c.limit, err, c.want)
		}
	}
	if i, err := mountIndex(spec, "/mnt/a"); i != 0 || err != nil {
		t.Errorf("mountIndex = %d, %v", i, err)
	}
	if _, err := mountIndex(spec, "/mnt/x"); err == nil {
		t.Error("mountIndex found a missing mount")
	}
}

func TestPortChecks(t *testing.T) {
	spec := &instance.Spec{Name: "a", VM: &instance.VMSpec{Ports: []instance.PortMapping{{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}}}}
	if proto, err := checkNewPort(spec, instance.PortMapping{HostPort: 8080, Protocol: "udp"}); err != nil || proto != "udp" {
		t.Errorf("checkNewPort udp = %q, %v", proto, err)
	}
	if _, err := checkNewPort(spec, instance.PortMapping{HostPort: 8080}); err == nil {
		t.Error("checkNewPort took a port already forwarded")
	}
	if i, err := portIndex(spec, 8080, "tcp"); i != 0 || err != nil {
		t.Errorf("portIndex = %d, %v", i, err)
	}
	if _, err := portIndex(spec, 8080, "udp"); err == nil {
		t.Error("portIndex found a missing forward")
	}
	bridged := &instance.Spec{Name: "b", VM: &instance.VMSpec{NetworkMode: "bridge"}}
	if _, err := checkNewPort(bridged, instance.PortMapping{HostPort: 1}); err == nil {
		t.Error("checkNewPort took a port on a bridged VM")
	}
}
