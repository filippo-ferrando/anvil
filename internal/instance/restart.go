package instance

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	restartBackoffMin = time.Second
	restartBackoffMax = time.Minute
	// restartStableAfter is how long an instance must run before its restart count resets.
	restartStableAfter = 10 * time.Minute
)

// restartTracker holds the restart policy's per-instance counters and pending timers.
type restartTracker struct {
	mu        sync.Mutex
	attempts  map[string]int
	lastStart map[string]time.Time
	timers    map[string]*time.Timer
}

func (t *restartTracker) init() {
	if t.attempts == nil {
		t.attempts = make(map[string]int)
		t.lastStart = make(map[string]time.Time)
		t.timers = make(map[string]*time.Timer)
	}
}

// started notes an explicit start: the restart count begins again from zero.
func (t *restartTracker) started(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.lastStart[id] = time.Now()
	delete(t.attempts, id)
}

// cancel drops a pending restart and the counters, e.g. because someone stopped the instance.
func (t *restartTracker) cancel(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	if timer := t.timers[id]; timer != nil {
		timer.Stop()
	}
	delete(t.timers, id)
	delete(t.attempts, id)
}

// forget cancels any pending restart and drops everything kept for a deleted instance.
func (t *restartTracker) forget(id string) {
	t.cancel(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.lastStart, id)
}

// restartDelay returns how long to wait before restart attempt number n (0-based).
func restartDelay(n int) time.Duration {
	d := restartBackoffMin
	for range n {
		d *= 2
		if d >= restartBackoffMax {
			return restartBackoffMax
		}
	}
	return d
}

// wantsRestart reports whether spec's policy asks for a restart after it stopped on its own.
func wantsRestart(spec *Spec) bool {
	if spec.UserStopped {
		return false
	}
	switch spec.RestartPolicy.Mode {
	case RestartAlways:
		return spec.State == StateStopped || spec.State == StateError
	case RestartOnFailure:
		return spec.State == StateError
	default:
		return false
	}
}

// maybeRestart schedules a restart of spec with exponential backoff if its policy asks
// for one. On-failure gives up after MaxRetries restarts that didn't run stably.
func (m *Manager) maybeRestart(spec *Spec) {
	if !wantsRestart(spec) {
		m.restarts.cancel(spec.ID)
		return
	}
	t := &m.restarts
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	if last, ok := t.lastStart[spec.ID]; ok && time.Since(last) > restartStableAfter {
		t.attempts[spec.ID] = 0
	}
	n := t.attempts[spec.ID]
	if p := spec.RestartPolicy; p.Mode == RestartOnFailure && p.MaxRetries > 0 && n >= p.MaxRetries {
		log.Printf("instance: %s failed %d times in a row, not restarting it again", spec.Name, n)
		m.record(context.Background(), spec, "restart", "", fmt.Errorf("failed %d times in a row, not restarting it again", n))
		return
	}
	delay := restartDelay(n)
	t.attempts[spec.ID] = n + 1
	if old := t.timers[spec.ID]; old != nil {
		old.Stop()
	}
	id := spec.ID
	t.timers[id] = time.AfterFunc(delay, func() { m.restartNow(id) })
	log.Printf("instance: restarting %s in %s (policy %s, attempt %d)", spec.Name, delay, spec.RestartPolicy, n+1)
}

// restartNow runs a scheduled restart, unless the instance changed in the meantime.
func (m *Manager) restartNow(id string) {
	m.restarts.mu.Lock()
	delete(m.restarts.timers, id)
	m.restarts.mu.Unlock()

	spec, err := m.registry.GetByID(id)
	if err != nil {
		return
	}
	// Waits for a running stop or delete, then re-checks the record it left behind.
	_ = m.lockAndRun(context.Background(), spec, true, func(spec *Spec) error {
		if !wantsRestart(spec) {
			return nil
		}
		err := m.startForPolicy(spec)
		m.record(context.Background(), spec, "restart", "policy "+spec.RestartPolicy.String(), err)
		if err != nil {
			log.Printf("instance: restarting %s: %v", spec.Name, err)
			spec.State = StateError
			m.maybeRestart(spec) // try again, later
		}
		return nil
	})
}

// startForPolicy starts spec without touching the counters an explicit Start resets.
func (m *Manager) startForPolicy(spec *Spec) error {
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	if err := b.Start(context.Background(), spec); err != nil {
		return err
	}
	m.restarts.mu.Lock()
	m.restarts.init()
	m.restarts.lastStart[spec.ID] = time.Now()
	m.restarts.mu.Unlock()
	spec.State = StateRunning
	return m.save(spec, func(s *Spec) { s.State = StateRunning })
}

// Autostart starts every instance marked autostart that isn't running and wasn't stopped
// on purpose. Call once at daemon startup, after Reconcile.
func (m *Manager) Autostart(ctx context.Context) {
	specs, err := m.registry.List("")
	if err != nil {
		log.Printf("instance: listing instances to autostart: %v", err)
		return
	}
	var names []string
	for _, s := range specs {
		if s.Autostart && !s.UserStopped && (s.State == StateStopped || s.State == StateError) {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	log.Printf("instance: autostarting %v", names)
	if err := m.start(ctx, names, "autostart"); err != nil {
		log.Printf("instance: autostart: %v", err)
	}
}
