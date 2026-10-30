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

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// WarmStore keeps the warm snapshot records; *store.Store is one.
type WarmStore interface {
	PutWarm(store.WarmSnapshot) error
	GetWarm(key string) (store.WarmSnapshot, error)
	ListWarm() ([]store.WarmSnapshot, error)
	DeleteWarm(key string) error
}

// Manager owns warm snapshots and clones. Engine is nil where clones can't run.
type Manager struct {
	Engine Engine
	Store  WarmStore
	Dir    func(key string) string // where warm snapshot key's files live

	mu       sync.Mutex
	building map[string]*build // warm key -> the build in progress
	clones   map[string]*cloneState
}

// build is one warm snapshot build that later callers for the same key wait on.
type build struct {
	done chan struct{}
	rec  store.WarmSnapshot
	err  error
}

func NewManager(engine Engine, s WarmStore, dir func(string) string) *Manager {
	return &Manager{Engine: engine, Store: s, Dir: dir, building: map[string]*build{}, clones: map[string]*cloneState{}}
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
	s, err := e.WarmSpec(p)
	if err != nil {
		return store.WarmSnapshot{}, WarmSpec{}, err
	}
	for {
		if !rebuild {
			if rec, err := m.Store.GetWarm(s.Key); err == nil && m.complete(s.Key) {
				rec.LastUsedAt = time.Now()
				_ = m.Store.PutWarm(rec)
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
	progress(fmt.Sprintf("building the %s warm snapshot (%s, %d MiB), once", s.Distro, cpus, s.MemoryMiB))
	start := time.Now()
	dir := m.Dir(s.Key)
	if err := e.BuildWarm(ctx, s, dir, progress); err != nil {
		return store.WarmSnapshot{}, fmt.Errorf("building the %s warm snapshot: %w", s.Distro, err)
	}
	now := time.Now()
	rec := store.WarmSnapshot{
		Key: s.Key, Distro: s.Distro, Arch: s.Arch, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, DiskGiB: s.DiskGiB,
		FirecrackerVersion: s.FirecrackerVersion, SizeBytes: diskUsage(dir), CreatedAt: now, LastUsedAt: now,
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
	var deleted []string
	for _, key := range keys {
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
