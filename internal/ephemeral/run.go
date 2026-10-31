package ephemeral

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// Event is one thing a run reports; exactly one field is set.
type Event struct {
	Progress *Progress
	Output   *Output
	Exit     *Exit
	App      *App // RunApp only, before the app starts
}

type Progress struct{ CloneID, Distro, Message string }

type Output struct {
	CloneID, Distro string
	Stderr          bool
	Data            []byte
}

// Exit ends one distro's part of a run. Code is -1 when the command never ran (Err says why).
type Exit struct {
	CloneID, Distro string
	Code            int
	Duration        time.Duration
	Kept            bool
	Err             string
}

// RunRequest is one `anvil launch --ephemeral` or `anvil matrix` run.
type RunRequest struct {
	Distros    []string
	Arch       string
	Argv, Env  []string
	Cwd        string
	CPUs       int
	MemoryMiB  int64
	KeepFailed bool
	Upload     string // a tar file extracted into /work in every clone, "" for none
	// WaitUpload, when set, waits until Upload is complete; the clones restore meanwhile.
	WaitUpload func(ctx context.Context) error
	// TTY, Stdin and Resize are for a run on one distro.
	TTY    bool
	Size   guestexec.Size
	Stdin  io.Reader
	Resize <-chan guestexec.Size
}

// cloneState is a clone a run started, tracked until it is deleted.
type cloneState struct {
	id, distro, warmKey string
	startedAt           time.Time
	clone               Clone
	deleting            bool // guarded by Manager.mu

	mu       sync.Mutex
	exited   bool
	exitCode int
	kept     bool
}

// CloneInfo describes a running or kept clone.
type CloneInfo struct {
	ID, Distro, WarmKey string
	StartedAt           time.Time
	Exited, Kept        bool
	ExitCode            int
}

// Run restores one clone per distro, all at once, runs the command in each and reports
// through emit, which is never called concurrently. Clones are deleted at the end, a failed
// one kept with KeepFailed; a cancelled ctx deletes them all.
func (m *Manager) Run(ctx context.Context, r RunRequest, emit func(Event)) error {
	if len(r.Distros) == 0 {
		return errors.New("no distro to run on")
	}
	if len(r.Argv) == 0 {
		return errors.New("no command to run")
	}
	if r.TTY && len(r.Distros) > 1 {
		return errors.New("a terminal is for a run on one distro")
	}
	e, err := m.engine()
	if err != nil {
		return err
	}
	var mu sync.Mutex
	safe := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		emit(ev)
	}
	var wg sync.WaitGroup
	for _, d := range r.Distros {
		wg.Go(func() { m.runOne(ctx, e, r, d, safe) })
	}
	wg.Wait()
	return nil
}

func (m *Manager) runOne(ctx context.Context, e Engine, r RunRequest, distro string, emit func(Event)) {
	id := "eph-" + ulid.Make().String()
	progress := func(msg string) { emit(Event{Progress: &Progress{CloneID: id, Distro: distro, Message: msg}}) }
	fail := func(err error) {
		emit(Event{Exit: &Exit{CloneID: id, Distro: distro, Code: -1, Err: err.Error()}})
	}
	_, spec, err := m.Warm(ctx, WarmParams{Distro: distro, Arch: r.Arch, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB}, false, progress)
	if err != nil {
		fail(err)
		return
	}
	start := time.Now()
	clone, err := m.restore(ctx, e, id, spec, progress)
	if err != nil {
		fail(err)
		return
	}
	id = clone.ID() // a pooled clone has its own
	st := &cloneState{id: id, distro: distro, warmKey: spec.Key, startedAt: start, clone: clone}
	m.mu.Lock()
	m.clones[id] = st
	m.mu.Unlock()

	progress("running: " + strings.Join(r.Argv, " "))
	code, runErr := m.runCommand(ctx, clone, r, func(stderr bool, data []byte) {
		emit(Event{Output: &Output{CloneID: id, Distro: distro, Stderr: stderr, Data: data}})
	})
	failed := runErr != nil || code != 0
	keep := r.KeepFailed && failed && ctx.Err() == nil
	st.mu.Lock()
	st.exited, st.exitCode, st.kept = true, code, keep
	st.mu.Unlock()
	exit := &Exit{CloneID: id, Distro: distro, Code: code, Duration: time.Since(start), Kept: keep}
	if runErr != nil {
		exit.Code, exit.Err = -1, runErr.Error()
	}
	// The result doesn't wait for the teardown; the clone counts as running until it's gone.
	if !keep && m.claimDelete(id) {
		m.teardown.Go(func() { m.removeClone(context.WithoutCancel(ctx), id) })
	}
	emit(Event{Exit: exit})
}

func (m *Manager) runCommand(ctx context.Context, clone Clone, r RunRequest, out func(stderr bool, data []byte)) (int, error) {
	req := guestexec.Request{
		Header: guestexec.Header{Argv: r.Argv, Env: r.Env, Cwd: r.Cwd, TTY: r.TTY, Rows: r.Size.Rows, Cols: r.Size.Cols},
		Stdin:  r.Stdin, Resize: r.Resize,
		Stdout: writerFunc(func(p []byte) { out(false, p) }),
		Stderr: writerFunc(func(p []byte) { out(true, p) }),
	}
	if r.Upload != "" {
		if r.WaitUpload != nil {
			if err := r.WaitUpload(ctx); err != nil {
				return -1, fmt.Errorf("receiving the upload: %w", err)
			}
		}
		f, err := os.Open(r.Upload)
		if err != nil {
			return -1, err
		}
		defer f.Close()
		req.Upload = f
	}
	return m.guestRun(ctx, clone, req)
}

// runInClone runs r in clone through a new connection to its anvil-guest.
func runInClone(ctx context.Context, clone Clone, r guestexec.Request) (int, error) {
	conn, err := clone.Dial(ctx)
	if err != nil {
		return -1, fmt.Errorf("reaching anvil-guest in the clone: %w", err)
	}
	return guestexec.Run(ctx, conn, r)
}

// writerFunc hands every write to a func; the data is copied, since it outlives the call.
type writerFunc func(p []byte)

func (w writerFunc) Write(p []byte) (int, error) {
	w(append([]byte(nil), p...))
	return len(p), nil
}

// deleteClone deletes clone id, which stays listed (and counted for memory) until it's
// gone. A clone already being deleted is left to that deletion.
func (m *Manager) deleteClone(ctx context.Context, id string) error {
	if !m.claimDelete(id) {
		return nil
	}
	return m.removeClone(ctx, id)
}

// claimDelete marks clone id as being deleted, false when someone already is.
func (m *Manager) claimDelete(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.clones[id]
	if st == nil {
		return true // not tracked (yet): the engine still cleans up what there is
	}
	if st.deleting {
		return false
	}
	st.deleting = true
	return true
}

func (m *Manager) removeClone(ctx context.Context, id string) error {
	err := m.Engine.DeleteClone(ctx, id)
	m.mu.Lock()
	delete(m.clones, id)
	m.mu.Unlock()
	return err
}

// WaitDeletes waits until the clones of finished runs are deleted.
func (m *Manager) WaitDeletes() { m.teardown.Wait() }

// Clones lists the clones of runs in progress and the kept ones, oldest first.
func (m *Manager) Clones() []CloneInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]CloneInfo, 0, len(m.clones))
	for _, st := range m.clones {
		st.mu.Lock()
		out = append(out, CloneInfo{ID: st.id, Distro: st.distro, WarmKey: st.warmKey, StartedAt: st.startedAt,
			Exited: st.exited, ExitCode: st.exitCode, Kept: st.kept})
		st.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// resolveClone finds the one clone whose id starts with prefix ("eph-" may be left out).
func (m *Manager) resolveClone(prefix string) (*cloneState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []*cloneState
	for id, st := range m.clones {
		if prefix != "" && (strings.HasPrefix(id, prefix) || strings.HasPrefix(id, "eph-"+prefix)) {
			found = append(found, st)
		}
	}
	switch {
	case len(found) == 0:
		return nil, fmt.Errorf("no clone %q: %w", prefix, instance.ErrNotFound)
	case len(found) > 1:
		return nil, fmt.Errorf("%q matches %d clones, give more of the id", prefix, len(found))
	}
	return found[0], nil
}

// Exec runs r in clone id through anvil-guest, `anvil ephemeral shell` among others.
func (m *Manager) Exec(ctx context.Context, id string, r guestexec.Request) (int, error) {
	st, err := m.resolveClone(id)
	if err != nil {
		return -1, err
	}
	conn, err := st.clone.Dial(ctx)
	if err != nil {
		return -1, fmt.Errorf("reaching anvil-guest in %s: %w", st.id, err)
	}
	return guestexec.Run(ctx, conn, r)
}

// DeleteClones deletes clones by id prefix, or all of them, running ones included.
func (m *Manager) DeleteClones(ctx context.Context, prefixes []string, all bool) ([]string, error) {
	var ids []string
	if all {
		for _, c := range m.Clones() {
			ids = append(ids, c.ID)
		}
	}
	for _, p := range prefixes {
		st, err := m.resolveClone(p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, st.id)
	}
	var deleted []string
	for _, id := range ids {
		if err := m.deleteClone(ctx, id); err != nil {
			return deleted, err
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}
