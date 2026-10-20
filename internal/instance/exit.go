package instance

import (
	"context"
	"log"
)

// hookExits records instances that change state on their own, which also emits a Watch event.
func (m *Manager) hookExits() {
	for _, b := range m.backends {
		en, ok := b.(ExitNotifier)
		if !ok {
			continue
		}
		en.SetExitHook(m.recordExit)
	}
}

// recordExit applies a state change made outside anvil. A stop only moves a running
// instance and a start only a stopped or errored one: any other record was already
// changed by whoever stopped, started or deleted it through anvil, and is left alone.
func (m *Manager) recordExit(instanceID string, state State, reason string) {
	spec, err := m.registry.GetByID(instanceID)
	if err != nil {
		return
	}
	// Waits for a running operation, so its outcome is the one the checks below see.
	_ = m.lockAndRun(context.Background(), spec, true, func(spec *Spec) error {
		m.applyExit(spec, state, reason)
		return nil
	})
}

// liveState asks spec's backend for its current state, "" when it can't tell.
func (m *Manager) liveState(spec *Spec) State {
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return ""
	}
	st, err := b.Status(context.Background(), spec)
	if err != nil {
		return ""
	}
	return st
}

func (m *Manager) applyExit(spec *Spec, state State, reason string) {
	switch {
	case state == StateRunning && (spec.State == StateStopped || spec.State == StateError):
	case state != StateRunning && (spec.State == StateRunning || spec.State == StateStarting):
	default:
		return
	}
	if state != StateRunning && m.liveState(spec) == StateRunning {
		return // a start finished after the exit was reported, so the report is stale
	}
	spec.State = state
	if err := m.save(spec, func(s *Spec) { s.State = state }); err != nil {
		log.Printf("instance: recording %s's new state %s: %v", spec.Name, state, err)
		return
	}
	action := "exit"
	if state == StateRunning {
		action = "start"
	}
	m.record(context.Background(), spec, action, reason, nil)
	if state != StateRunning {
		m.maybeRestart(spec)
	}
}
