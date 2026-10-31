package ephemeral

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// WarmStore keeps the warm snapshot records; *store.Store is one.
type WarmStore interface {
	PutWarm(store.WarmSnapshot) error
	GetWarm(key string) (store.WarmSnapshot, error)
	ListWarm() ([]store.WarmSnapshot, error)
	DeleteWarm(key string) error
	PutWarmPool(store.WarmPool) error
	ListWarmPools() ([]store.WarmPool, error)
	DeleteWarmPool(id string) error
}

// Manager owns warm snapshots and clones. Engine is nil where clones can't run.
type Manager struct {
	Engine Engine
	Store  WarmStore
	Dir    func(key string) string // where warm snapshot key's files live

	mu       sync.Mutex
	building map[string]*build // warm key -> the build in progress
	clones   map[string]*cloneState
	pools    map[string]*pool // store.WarmPool.ID() -> its clones
	teardown sync.WaitGroup   // clones of finished runs being deleted

	// guestRun runs a command in a clone; tests replace it.
	guestRun func(ctx context.Context, clone Clone, r guestexec.Request) (int, error)

	admitMu   sync.Mutex
	restoring int64 // MiB of clones admitted and still restoring
	// memAvailable is the host's available memory in MiB; tests replace it.
	memAvailable func() (int64, error)
	// wake readies a pooled clone as it is handed out; tests replace it.
	wake func(ctx context.Context, c Clone) error
}

// build is one warm snapshot build that later callers for the same key wait on.
type build struct {
	done chan struct{}
	rec  store.WarmSnapshot
	err  error
}

func NewManager(engine Engine, s WarmStore, dir func(string) string) *Manager {
	return &Manager{Engine: engine, Store: s, Dir: dir, building: map[string]*build{}, clones: map[string]*cloneState{}, pools: map[string]*pool{},
		guestRun: runInClone, memAvailable: hostMemAvailable, wake: setClock}
}

func (m *Manager) engine() (Engine, error) {
	if m.Engine == nil {
		return nil, ErrUnsupported
	}
	return m.Engine, nil
}

// Warm returns the warm snapshot for p, building it first when there is none (or rebuild).
// Callers asking for one that is being built wait for that build.
func (m *Manager) Warm(ctx context.Context, p WarmParams, rebuild bool, progress func(string)) (store.WarmSnapshot, WarmSpec, error) {
	if progress == nil {
		progress = func(string) {}
	}
	e, err := m.engine()
	if err != nil {
		return store.WarmSnapshot{}, WarmSpec{}, err
	}
	if key, ok := warmRef(p.Distro); ok {
		if p.Setup != nil || p.App != "" {
			return store.WarmSnapshot{}, WarmSpec{}, errors.New("a warm snapshot picked by key takes no setup script or app")
		}
		return m.warmByKey(key)
	}
	s, err := e.WarmSpec(p)
	if err == nil && p.App != "" {
		s, err = withApp(s, p)
	}
	if err == nil && p.Setup != nil {
		if p.App != "" {
			return store.WarmSnapshot{}, WarmSpec{}, errors.New("a warm snapshot with an app takes no setup script")
		}
		// The plain snapshot first: a setup build starts from the same image and size.
		var base store.WarmSnapshot
		if base, _, err = m.Warm(ctx, WarmParams{Distro: p.Distro, Arch: p.Arch, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB}, false, progress); err == nil && base.Key != s.Key {
			err = fmt.Errorf("the %s warm snapshot changed while resolving it", s.Distro)
		}
		if err == nil {
			s, err = withSetup(s, p.Setup)
		}
	}
	if err != nil {
		return store.WarmSnapshot{}, WarmSpec{}, err
	}
	for {
		if !rebuild {
			if rec, err := m.Store.GetWarm(s.Key); err == nil && m.complete(s.Key) {
				// A write (and its fsync) at most once a minute: the listing shows minutes.
				if now := time.Now(); now.Sub(rec.LastUsedAt) >= time.Minute {
					rec.LastUsedAt = now
					_ = m.Store.PutWarm(rec)
				}
				return rec, s, nil
			}
		}
		m.mu.Lock()
		if b := m.building[s.Key]; b != nil {
			m.mu.Unlock()
			progress("waiting for another build of the " + s.Distro + " warm snapshot")
			select {
			case <-b.done:
			case <-ctx.Done():
				return store.WarmSnapshot{}, s, ctx.Err()
			}
			if b.err != nil {
				return store.WarmSnapshot{}, s, b.err
			}
			rebuild = false
			continue
		}
		b := &build{done: make(chan struct{})}
		m.building[s.Key] = b
		m.mu.Unlock()

		b.rec, b.err = m.buildWarm(ctx, e, s, progress)
		m.mu.Lock()
		delete(m.building, s.Key)
		m.mu.Unlock()
		close(b.done)
		return b.rec, s, b.err
	}
}

func (m *Manager) buildWarm(ctx context.Context, e Engine, s WarmSpec, progress func(string)) (store.WarmSnapshot, error) {
	cpus := "1 CPU"
	if s.CPUs != 1 {
		cpus = fmt.Sprintf("%d CPUs", s.CPUs)
	}
	what := s.Distro
	var setup Setup
	switch {
	case s.App != "":
		what, setup = s.Distro+" + "+s.App, appSetup(s)
	case s.Setup != nil:
		what, setup = s.Distro+" + "+s.SetupName, scriptSetup(s)
	}
	progress(fmt.Sprintf("building the %s warm snapshot (%s, %d MiB), once", what, cpus, s.MemoryMiB))
	start := time.Now()
	dir := m.Dir(s.Key)
	if err := e.BuildWarm(ctx, s, dir, setup, progress); err != nil {
		return store.WarmSnapshot{}, fmt.Errorf("building the %s warm snapshot: %w", s.Distro, err)
	}
	now := time.Now()
	rec := store.WarmSnapshot{
		Key: s.Key, Distro: s.Distro, Arch: s.Arch, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, DiskGiB: s.DiskGiB,
		FirecrackerVersion: s.FirecrackerVersion, SizeBytes: diskUsage(dir), CreatedAt: now, LastUsedAt: now,
		App: s.App, X11: s.X11, User: s.User, Base: s.Base, Setup: s.SetupName,
	}
	if err := m.Store.PutWarm(rec); err != nil {
		return store.WarmSnapshot{}, err
	}
	progress(fmt.Sprintf("built the %s warm snapshot in %s", s.Distro, time.Since(start).Round(time.Second)))
	return rec, nil
}

// complete reports whether warm snapshot key's files are all there.
func (m *Manager) complete(key string) bool {
	for _, name := range []string{"vmstate", "mem", "disk.raw"} {
		if _, err := os.Stat(filepath.Join(m.Dir(key), name)); err != nil {
			return false
		}
	}
	return true
}

// diskUsage is the space dir's files take, counting sparse files by their blocks.
func diskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += blocks(info)
			}
		}
		return nil
	})
	return total
}

func (m *Manager) ListWarm() ([]store.WarmSnapshot, error) { return m.Store.ListWarm() }

// resolveWarm finds the one warm snapshot whose key starts with prefix.
func (m *Manager) resolveWarm(prefix string) (store.WarmSnapshot, error) {
	all, err := m.Store.ListWarm()
	if err != nil {
		return store.WarmSnapshot{}, err
	}
	var found []store.WarmSnapshot
	for _, w := range all {
		if strings.HasPrefix(w.Key, prefix) {
			found = append(found, w)
		}
	}
	switch {
	case prefix == "" || len(found) == 0:
		return store.WarmSnapshot{}, fmt.Errorf("no warm snapshot %q: %w", prefix, instance.ErrNotFound)
	case len(found) > 1:
		return store.WarmSnapshot{}, fmt.Errorf("%q matches %d warm snapshots, give more of the key", prefix, len(found))
	}
	return found[0], nil
}

// DeleteWarm removes warm snapshots by key prefix, or all of them. Running clones keep
// working: their jails hold links to the files.
func (m *Manager) DeleteWarm(prefixes []string, all bool) ([]string, error) {
	var keys []string
	if all {
		list, err := m.Store.ListWarm()
		if err != nil {
			return nil, err
		}
		for _, w := range list {
			keys = append(keys, w.Key)
		}
	}
	for _, p := range prefixes {
		w, err := m.resolveWarm(p)
		if err != nil {
			return nil, err
		}
		keys = append(keys, w.Key)
	}
	keys, err := m.withDerived(keys)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, key := range keys {
		if err := m.dropPoolsOf(key); err != nil {
			return deleted, err
		}
		if err := os.RemoveAll(m.Dir(key)); err != nil {
			return deleted, err
		}
		if err := m.Store.DeleteWarm(key); err != nil && !errors.Is(err, instance.ErrNotFound) {
			return deleted, err
		}
		deleted = append(deleted, key)
	}
	return deleted, nil
}

// withDerived adds to keys every warm snapshot built on one of them, setup and saved
// ones, which can't outlive their base. Each key appears once.
func (m *Manager) withDerived(keys []string) ([]string, error) {
	all, err := m.Store.ListWarm()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for len(keys) > 0 {
		key := keys[0]
		keys = keys[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
		for _, w := range all {
			if w.Base == key {
				keys = append(keys, w.Key)
			}
		}
	}
	return out, nil
}

// Cleanup runs once at anvild's start: clones and builds of the previous anvild are gone
// with it, and so is a warm dir no record points at (a build cut short).
func (m *Manager) Cleanup(ctx context.Context) {
	if m.Engine == nil {
		return
	}
	if err := m.Engine.SweepClones(ctx); err != nil {
		log.Printf("ephemeral: removing leftover clones: %v", err)
	}
	parent := filepath.Dir(m.Dir("x"))
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, err := m.Store.GetWarm(e.Name()); err != nil {
			log.Printf("ephemeral: removing unfinished warm snapshot %s", e.Name())
			_ = os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
}
