package instance

import (
	"context"
	"fmt"
)

// withGuest fills spec.Guest from its backend's cache, if that backend tracks guests.
func (m *Manager) withGuest(spec *Spec) *Spec {
	if spec == nil {
		return nil
	}
	if gi, ok := m.backends[spec.Kind].(GuestInspector); ok {
		if info, ok := gi.GuestInfo(spec); ok {
			spec.Guest = &info
		}
	}
	return spec
}

func (m *Manager) withGuests(specs []*Spec) []*Spec {
	for _, s := range specs {
		m.withGuest(s)
	}
	return specs
}

// hookGuestChanges turns a backend's guest cache changes into Watch events.
func (m *Manager) hookGuestChanges() {
	for _, b := range m.backends {
		gi, ok := b.(GuestInspector)
		if !ok {
			continue
		}
		gi.SetGuestChangeHook(func(instanceID string) {
			spec, err := m.registry.GetByID(instanceID)
			if err != nil {
				return
			}
			m.events.publish(Event{Type: EventUpdated, Spec: m.withGuest(spec)})
		})
	}
}

// WaitReady blocks until name's first-boot setup finished (cloud-init for a VM).
// A kind with no such setup is ready as soon as it runs.
func (m *Manager) WaitReady(ctx context.Context, name string, progress func(status string)) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	if spec.State != StateRunning {
		return fmt.Errorf("instance: %s is not running", name)
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	rw, ok := b.(ReadyWaiter)
	if !ok {
		return nil
	}
	return rw.WaitReady(ctx, spec, progress)
}
