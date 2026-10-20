package instance

import (
	"errors"
	"fmt"
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

// withLock runs fn on a fresh copy of spec's record while holding its instance lock.
// A second caller fails fast with ErrBusy instead of queueing behind a slow operation.
func (m *Manager) withLock(spec *Spec, fn func(*Spec) error) error {
	return m.lockAndRun(spec, false, fn)
}

// lockAndRun is withLock, waiting for the lock instead when wait is set.
func (m *Manager) lockAndRun(spec *Spec, wait bool, fn func(*Spec) error) error {
	release, err := m.locks.acquire(spec.ID, wait)
	if err != nil {
		return fmt.Errorf("instance: %s: %w", spec.Name, err)
	}
	defer release()
	// Re-read: the record may have changed, or been deleted, before the lock was taken.
	fresh, err := m.registry.GetByID(spec.ID)
	if err != nil {
		return err
	}
	return fn(fresh)
}

// reserveName holds name for a Launch or Fork until release, and fails if it's taken.
func (m *Manager) reserveName(name string) (release func(), err error) {
	release, err = m.names.acquire(name, false)
	if err != nil {
		return nil, fmt.Errorf("instance: name %q is already in use", name)
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
