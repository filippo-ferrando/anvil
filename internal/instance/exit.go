package instance

import "log"

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
func (m *Manager) recordExit(instanceID string, state State) {
	spec, err := m.registry.GetByID(instanceID)
	if err != nil {
		return
	}
	switch {
	case state == StateRunning && (spec.State == StateStopped || spec.State == StateError):
	case state != StateRunning && (spec.State == StateRunning || spec.State == StateStarting):
	default:
		return
	}
	spec.State = state
	if err := m.registry.PutInstance(spec); err != nil {
		log.Printf("instance: recording %s's new state %s: %v", spec.Name, state, err)
	}
}
