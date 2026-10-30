//go:build linux

package ephemeral

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// fakeEngine builds warm snapshots as empty files, and serves each clone's anvil-guest
// with the real guestexec server, here on the host.
type fakeEngine struct {
	builds   atomic.Int32
	setups   atomic.Int32 // builds with a setup
	buildErr error
	mu       sync.Mutex
	deleted  []string
	swept    bool
}

func (f *fakeEngine) WarmSpec(p WarmParams) (WarmSpec, error) {
	if p.Distro == "unknown" {
		return WarmSpec{}, errors.New("no such distro")
	}
	return WarmSpec{Key: "k" + p.Distro + fmt.Sprint(p.CPUs), Distro: p.Distro, CPUs: p.CPUs, MemoryMiB: 512}, nil
}

func (f *fakeEngine) BuildWarm(_ context.Context, s WarmSpec, dir string, setup Setup, progress func(string)) error {
	f.builds.Add(1)
	if setup != nil {
		f.setups.Add(1)
	}
	time.Sleep(50 * time.Millisecond) // long enough for concurrent callers to pile up
	if f.buildErr != nil {
		return f.buildErr
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, name := range []string{"vmstate", "mem", "disk.raw"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o640); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeEngine) RestoreClone(_ context.Context, id string, s WarmSpec, dir string) (Clone, error) {
	if s.Distro == "broken" {
		return nil, errors.New("restore failed")
	}
	return &fakeClone{id: id, started: time.Now()}, nil
}

func (f *fakeEngine) DeleteClone(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeEngine) SweepClones(context.Context) error { f.swept = true; return nil }

type fakeClone struct {
	id      string
	started time.Time
	vsock   string // where Listen listens, set by Listen
}

func (c *fakeClone) ID() string           { return c.id }
func (c *fakeClone) StartedAt() time.Time { return c.started }
func (c *fakeClone) Listen(port uint32) (net.Listener, error) {
	dir, err := os.MkdirTemp("", "fakeclone")
	if err != nil {
		return nil, err
	}
	c.vsock = filepath.Join(dir, fmt.Sprintf("vsock_%d", port))
	return net.Listen("unix", c.vsock)
}

func (c *fakeClone) Dial(context.Context) (io.ReadWriteCloser, error) {
	host, guest := net.Pipe()
	go func() { _ = guestexec.Serve(guest) }()
	return host, nil
}

func newTestManager(t *testing.T) (*Manager, *fakeEngine) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	root := t.TempDir()
	e := &fakeEngine{}
	return NewManager(e, s, func(key string) string { return filepath.Join(root, "warm", key) }), e
}

func TestWarmBuildsOnce(t *testing.T) {
	m, e := newTestManager(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	keys := make([]string, 8)
	for i := range keys {
		wg.Go(func() {
			rec, _, err := m.Warm(ctx, WarmParams{Distro: "ubuntu", CPUs: 2}, false, nil)
			if err != nil {
				t.Error(err)
			}
			keys[i] = rec.Key
		})
	}
	wg.Wait()
	if n := e.builds.Load(); n != 1 {
		t.Fatalf("%d builds for 8 concurrent callers, want 1", n)
	}
	for _, k := range keys {
		if k != "kubuntu2" {
			t.Errorf("keys = %v", keys)
			break
		}
	}
	if rec, _, _ := m.Warm(ctx, WarmParams{Distro: "ubuntu", CPUs: 2}, false, nil); rec.SizeBytes == 0 || e.builds.Load() != 1 {
		t.Errorf("reuse = %+v, %d builds", rec, e.builds.Load())
	}
	if _, _, err := m.Warm(ctx, WarmParams{Distro: "ubuntu", CPUs: 2}, true, nil); err != nil || e.builds.Load() != 2 {
		t.Errorf("rebuild: %v, %d builds", err, e.builds.Load())
	}
	// A record whose files are gone is built again.
	_ = os.Remove(filepath.Join(m.Dir("kubuntu2"), "mem"))
	if _, _, err := m.Warm(ctx, WarmParams{Distro: "ubuntu", CPUs: 2}, false, nil); err != nil || e.builds.Load() != 3 {
		t.Errorf("after losing a file: %v, %d builds", err, e.builds.Load())
	}

	e.buildErr = errors.New("no network")
	if _, _, err := m.Warm(ctx, WarmParams{Distro: "debian", CPUs: 2}, false, nil); err == nil || !strings.Contains(err.Error(), "no network") {
		t.Errorf("failed build = %v", err)
	}
	if _, err := m.Store.GetWarm("kdebian2"); err == nil {
		t.Error("a failed build left a record")
	}
}

func TestDeleteWarmAndCleanup(t *testing.T) {
	m, e := newTestManager(t)
	ctx := context.Background()
	for _, d := range []string{"ubuntu", "uplift", "alpine"} {
		if _, _, err := m.Warm(ctx, WarmParams{Distro: d, CPUs: 1}, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.DeleteWarm([]string{"ku"}, false); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Errorf("ambiguous prefix = %v", err)
	}
	if got, err := m.DeleteWarm([]string{"kalp"}, false); err != nil || !slices.Equal(got, []string{"kalpine1"}) {
		t.Errorf("DeleteWarm = %v, %v", got, err)
	}
	if _, err := os.Stat(m.Dir("kalpine1")); !os.IsNotExist(err) {
		t.Errorf("files left after delete: %v", err)
	}

	orphan := m.Dir("orphan.part")
	if err := os.MkdirAll(orphan, 0o750); err != nil {
		t.Fatal(err)
	}
	m.Cleanup(ctx)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) || !e.swept {
		t.Errorf("Cleanup kept %s (%v) or didn't sweep clones (%v)", orphan, err, e.swept)
	}
	if !m.complete("kubuntu1") {
		t.Error("Cleanup removed a recorded warm snapshot")
	}
	if got, _ := m.DeleteWarm(nil, true); len(got) != 2 {
		t.Errorf("delete all = %v", got)
	}
}

// collect runs r and returns its events, grouped by kind.
func collect(t *testing.T, ctx context.Context, m *Manager, r RunRequest) (exits map[string]Exit, out map[string]string) {
	t.Helper()
	exits, out = map[string]Exit{}, map[string]string{}
	err := m.Run(ctx, r, func(ev Event) {
		switch {
		case ev.Exit != nil:
			exits[ev.Exit.Distro] = *ev.Exit
		case ev.Output != nil && ev.Output.Stderr:
			// stdout and stderr are separate pipes with no order between them.
			out[ev.Output.Distro+"/stderr"] += string(ev.Output.Data)
		case ev.Output != nil:
			out[ev.Output.Distro] += string(ev.Output.Data)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return exits, out
}

func TestRunMatrix(t *testing.T) {
	m, e := newTestManager(t)
	guestexec.WorkDir = filepath.Join(t.TempDir(), "work")
	t.Cleanup(func() { guestexec.WorkDir = "/work" })

	upload := filepath.Join(t.TempDir(), "upload.tar")
	f, _ := os.Create(upload)
	tw := tar.NewWriter(f)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "hello", Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("hi\n"))
	_ = tw.Close()
	f.Close()

	exits, out := collect(t, context.Background(), m, RunRequest{
		Distros: []string{"ubuntu", "alpine", "broken", "unknown"},
		Argv:    []string{"sh", "-c", "cat hello; echo oops >&2; exit 3"}, Upload: upload,
	})
	for _, d := range []string{"ubuntu", "alpine"} {
		if ex := exits[d]; ex.Code != 3 || ex.Err != "" || ex.Kept || out[d] != "hi\n" || out[d+"/stderr"] != "oops\n" {
			t.Errorf("%s: exit %+v, stdout %q, stderr %q", d, ex, out[d], out[d+"/stderr"])
		}
	}
	for d, want := range map[string]string{"broken": "restore failed", "unknown": "no such distro"} {
		if ex := exits[d]; ex.Code != -1 || !strings.Contains(ex.Err, want) {
			t.Errorf("%s: exit %+v, want an error with %q", d, ex, want)
		}
	}
	if len(m.Clones()) != 0 || len(e.deleted) != 2 {
		t.Errorf("after the run: clones %v, deleted %v", m.Clones(), e.deleted)
	}

	if err := m.Run(context.Background(), RunRequest{Distros: []string{"a", "b"}, Argv: []string{"true"}, TTY: true}, func(Event) {}); err == nil {
		t.Error("a terminal on two distros was accepted")
	}
}

func TestRunKeepFailedAndExec(t *testing.T) {
	m, e := newTestManager(t)
	ctx := context.Background()
	exits, _ := collect(t, ctx, m, RunRequest{Distros: []string{"ok", "bad"}, Argv: []string{"sh", "-c", `[ "$D" = x ]`}, Env: []string{"D=x"}, KeepFailed: true})
	if exits["ok"].Kept || exits["ok"].Code != 0 {
		t.Errorf("ok = %+v", exits["ok"])
	}
	exits, _ = collect(t, ctx, m, RunRequest{Distros: []string{"bad"}, Argv: []string{"false"}, KeepFailed: true})
	kept := exits["bad"]
	if !kept.Kept || kept.Code != 1 {
		t.Fatalf("bad = %+v, want kept", kept)
	}
	clones := m.Clones()
	if len(clones) != 1 || clones[0].ID != kept.CloneID || !clones[0].Exited || !clones[0].Kept || clones[0].ExitCode != 1 {
		t.Fatalf("Clones = %+v", clones)
	}

	var stdout strings.Builder
	code, err := m.Exec(ctx, strings.TrimPrefix(kept.CloneID, "eph-")[:10], guestexec.Request{
		Header: guestexec.Header{Argv: []string{"echo", "inside"}}, Stdout: &stdout})
	if err != nil || code != 0 || stdout.String() != "inside\n" {
		t.Errorf("Exec = %d, %v, %q", code, err, stdout.String())
	}
	if _, err := m.Exec(ctx, "nope", guestexec.Request{}); err == nil {
		t.Error("Exec in a missing clone worked")
	}
	if got, err := m.DeleteClones(ctx, nil, true); err != nil || len(got) != 1 || len(m.Clones()) != 0 {
		t.Errorf("DeleteClones = %v, %v; left %v", got, err, m.Clones())
	}
	if !slices.Contains(e.deleted, kept.CloneID) {
		t.Errorf("the engine never deleted %s: %v", kept.CloneID, e.deleted)
	}
}

func TestRunCancelDeletesClones(t *testing.T) {
	m, e := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx, RunRequest{Distros: []string{"a", "b"}, Argv: []string{"sh", "-c", "echo up; sleep 30"}, KeepFailed: true},
			func(ev Event) {
				if ev.Output != nil {
					started <- struct{}{}
				}
			})
	}()
	<-started
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after the cancel")
	}
	if len(m.Clones()) != 0 || len(e.deleted) != 2 {
		t.Errorf("after a cancel: clones %v, deleted %v (keep_failed doesn't apply)", m.Clones(), e.deleted)
	}
}
