package instance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// ErrBusy means another operation on the same instance (or launch of the same name) is still running.
var ErrBusy = errors.New("instance busy")

// keyLocks is a set of named locks: one per instance ID, or one per name being created.
type keyLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{} // closed when released
}

// acquire takes key's lock. When it's held, it fails with ErrBusy, or waits if wait is set.
func (l *keyLocks) acquire(key string, wait bool) (release func(), err error) {
	l.mu.Lock()
	for {
		ch, ok := l.held[key]
		if !ok {
			break
		}
		if !wait {
			l.mu.Unlock()
			return nil, ErrBusy
		}
		l.mu.Unlock()
		<-ch
		l.mu.Lock()
	}
	if l.held == nil {
		l.held = make(map[string]chan struct{})
	}
	ch := make(chan struct{})
	l.held[key] = ch
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.held, key)
		l.mu.Unlock()
		close(ch)
	}, nil
}

// keys returns every key currently held.
func (l *keyLocks) keys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Collect(maps.Keys(l.held))
}

// Creating returns the names of instances a Launch, Fork or import is still creating.
func (m *Manager) Creating() []string { return m.names.keys() }

// withLock runs fn on a fresh copy of spec's record while holding its instance lock.
// A second caller fails fast with ErrBusy instead of queueing behind a slow operation.
func (m *Manager) withLock(ctx context.Context, spec *Spec, fn func(*Spec) error) error {
	return m.lockAndRun(ctx, spec, false, fn)
}

// lockAndRun is withLock, waiting for the lock instead when wait is set. An instance
// held through ctx (see Hold) is already locked by the caller.
func (m *Manager) lockAndRun(ctx context.Context, spec *Spec, wait bool, fn func(*Spec) error) error {
	if !heldIDs(ctx)[spec.ID] {
		release, err := m.locks.acquire(spec.ID, wait)
		if err != nil {
			return fmt.Errorf("instance: %s: %w", spec.Name, err)
		}
		defer release()
	}
	// Re-read: the record may have changed, or been deleted, before the lock was taken.
	fresh, err := m.registry.GetByID(spec.ID)
	if err != nil {
		return err
	}
	return fn(fresh)
}

type holdKey struct{}

func heldIDs(ctx context.Context) map[string]bool {
	held, _ := ctx.Value(holdKey{}).(map[string]bool)
	return held
}

// Hold locks names' instances for a multi-step operation such as export or migration.
// Manager calls made with the returned ctx skip those locks; release frees them.
func (m *Manager) Hold(ctx context.Context, names []string) (context.Context, func(), error) {
	if len(names) == 0 {
		return ctx, func() {}, nil
	}
	specs, err := m.resolve(names)
	if err != nil {
		return nil, nil, err
	}
	held := maps.Clone(heldIDs(ctx))
	if held == nil {
		held = map[string]bool{}
	}
	var releases []func()
	release := func() {
		for _, r := range releases {
			r()
		}
	}
	for _, s := range specs {
		if held[s.ID] {
			continue
		}
		r, err := m.locks.acquire(s.ID, false)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("instance: %s: %w", s.Name, err)
		}
		releases = append(releases, r)
		held[s.ID] = true
	}
	return context.WithValue(ctx, holdKey{}, held), release, nil
}

type reservedKey struct{}

// ReserveNames holds names for a multi-step create such as import, and fails if one is
// taken. Launch with the returned ctx uses the reservation; release frees it.
func (m *Manager) ReserveNames(ctx context.Context, names []string) (context.Context, func(), error) {
	reserved := maps.Clone(reservedNames(ctx))
	if reserved == nil {
		reserved = map[string]bool{}
	}
	var releases []func()
	release := func() {
		for _, r := range releases {
			r()
		}
	}
	for _, name := range names {
		r, err := m.reserveName(ctx, name)
		if err != nil {
			release()
			return nil, nil, err
		}
		releases = append(releases, r)
		reserved[name] = true
	}
	return context.WithValue(ctx, reservedKey{}, reserved), release, nil
}

func reservedNames(ctx context.Context) map[string]bool {
	r, _ := ctx.Value(reservedKey{}).(map[string]bool)
	return r
}

// reserveName holds name for a Launch or Fork until release, and fails if it's taken.
// A name reserved through ctx (see ReserveNames) is the caller's already.
func (m *Manager) reserveName(ctx context.Context, name string) (release func(), err error) {
	release = func() {}
	if !reservedNames(ctx)[name] {
		if release, err = m.names.acquire(name, false); err != nil {
			return nil, fmt.Errorf("instance: name %q is already in use", name)
		}
	}
	if _, err := m.registry.GetByName(name); err == nil {
		release()
		return nil, fmt.Errorf("instance: name %q is already in use", name)
	} else if !errors.Is(err, ErrNotFound) {
		release()
		return nil, err
	}
	return release, nil
}

// save writes back the backend-owned VM/Container parts of spec, plus whatever set changes.
// Other fields, State included, keep their stored value unless set changes them.
func (m *Manager) save(spec *Spec, set func(*Spec)) error {
	return m.registry.UpdateInstance(spec.ID, func(s *Spec) error {
		s.VM, s.Container = spec.VM, spec.Container
		if set != nil {
			set(s)
		}
		return nil
	})
}
