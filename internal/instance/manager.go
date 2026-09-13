package instance

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/oklog/ulid/v2"
)

// Manager orchestrates Registry (persistence) and the per-Kind Backend
// implementations, presenting the single API internal/daemon's gRPC
// handlers call into. It has no gRPC/protobuf awareness — that conversion
// lives in internal/daemon.
type Manager struct {
	registry Registry
	backends map[Kind]Backend
}

func NewManager(registry Registry, backends map[Kind]Backend) *Manager {
	return &Manager{registry: registry, backends: backends}
}

// Reconcile re-derives the live state of every instance the registry
// thinks is running or starting, correcting drift from a daemon restart
// (a VM that's actually still alive, or one that silently died while
// anvild was down) rather than blindly trusting the registry. Call this
// once at daemon startup, before serving requests.
//
// Backends that don't implement Reconciler are left alone — their
// instances keep whatever state the registry already has.
func (m *Manager) Reconcile(ctx context.Context) error {
	specs, err := m.registry.List("")
	if err != nil {
		return fmt.Errorf("instance: listing instances to reconcile: %w", err)
	}
	for _, spec := range specs {
		if spec.State != StateRunning && spec.State != StateStarting {
			continue
		}
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			continue // no backend registered for this kind (yet) — leave state as-is
		}
		reconciler, ok := b.(Reconciler)
		if !ok {
			continue
		}

		newState, rErr := reconciler.Reconcile(ctx, spec)
		if rErr != nil {
			log.Printf("instance: reconciling %s (%s): %v", spec.Name, spec.ID, rErr)
		}
		if newState == spec.State {
			continue
		}
		spec.State = newState
		if err := m.registry.PutInstance(spec); err != nil {
			return fmt.Errorf("instance: persisting reconciled state for %s: %w", spec.Name, err)
		}
	}
	return nil
}

func (m *Manager) backendFor(kind Kind) (Backend, error) {
	b, ok := m.backends[kind]
	if !ok {
		return nil, fmt.Errorf("instance: no backend registered for kind %q", kind)
	}
	return b, nil
}

// LaunchParams is Manager.Launch's input, already validated/translated
// from the wire request by internal/daemon.
type LaunchParams struct {
	Name      string
	Kind      Kind
	VM        *VMSpec
	Container *ContainerSpec
	NoStart   bool
}

// LaunchEvent is one step of Launch's progress callback. Exactly one of
// Status, Instance, or Err is meaningful per call: Status for an
// in-progress phase description, Instance on successful completion (the
// final event), Err on failure (also final).
type LaunchEvent struct {
	Status   string
	Instance *Spec
	Err      error
}

// Launch provisions (and, unless params.NoStart, starts) a new instance,
// reporting progress via progress. It returns the same terminal error (if
// any) that was already reported through progress, so callers that only
// care about success/failure don't have to inspect events.
func (m *Manager) Launch(ctx context.Context, params LaunchParams, progress func(LaunchEvent)) error {
	b, err := m.backendFor(params.Kind)
	if err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}

	spec := &Spec{
		ID:        ulid.Make().String(),
		Name:      params.Name,
		Kind:      params.Kind,
		State:     StateStarting,
		CreatedAt: time.Now(),
		Labels:    map[string]string{},
		VM:        params.VM,
		Container: params.Container,
	}

	progress(LaunchEvent{Status: "provisioning"})
	if err := b.Create(ctx, spec); err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}

	if params.NoStart {
		spec.State = StateStopped
	} else {
		progress(LaunchEvent{Status: "starting"})
		if err := b.Start(ctx, spec); err != nil {
			progress(LaunchEvent{Err: err})
			return err
		}
		spec.State = StateRunning
	}

	if err := m.registry.PutInstance(spec); err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}

	progress(LaunchEvent{Instance: spec})
	return nil
}

func (m *Manager) List(kindFilter Kind) ([]*Spec, error) {
	return m.registry.List(kindFilter)
}

// resolve looks up each name, tolerating a mix of names in the input.
func (m *Manager) resolve(names []string) ([]*Spec, error) {
	if len(names) == 0 {
		return m.registry.List("")
	}
	specs := make([]*Spec, 0, len(names))
	for _, name := range names {
		spec, err := m.registry.GetByName(name)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func (m *Manager) Info(names []string) ([]*Spec, error) {
	return m.resolve(names)
}

// Logs streams name's log output — a VM's boot/console output, or (once M3
// lands) a container's actual stdout/stderr, see Backend.Logs — to send.
func (m *Manager) Logs(ctx context.Context, name string, follow bool, tailLines int, send func([]byte) error) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	return b.Logs(ctx, spec, follow, tailLines, send)
}

// Mount shares hostPath into name's guest at guestPath — see the
// Mounter interface's doc comment for why this isn't just part of Backend.
func (m *Manager) Mount(ctx context.Context, name, hostPath, guestPath string, readOnly bool) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	mounter, ok := b.(Mounter)
	if !ok {
		return fmt.Errorf("instance: %s instances don't support mounts", spec.Kind)
	}
	if err := mounter.Mount(ctx, spec, hostPath, guestPath, readOnly); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

// Umount removes a mount previously added with Mount.
func (m *Manager) Umount(ctx context.Context, name, guestPath string) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	mounter, ok := b.(Mounter)
	if !ok {
		return fmt.Errorf("instance: %s instances don't support mounts", spec.Kind)
	}
	if err := mounter.Umount(ctx, spec, guestPath); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

func (m *Manager) Start(ctx context.Context, names []string) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			return err
		}
		if err := b.Start(ctx, spec); err != nil {
			return fmt.Errorf("instance: starting %s: %w", spec.Name, err)
		}
		spec.State = StateRunning
		if err := m.registry.PutInstance(spec); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, names []string, force bool, timeout time.Duration) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			return err
		}
		if err := b.Stop(ctx, spec, force, timeout); err != nil {
			return fmt.Errorf("instance: stopping %s: %w", spec.Name, err)
		}
		spec.State = StateStopped
		if err := m.registry.PutInstance(spec); err != nil {
			return err
		}
	}
	return nil
}

// Delete tears down each named instance's backend resources. When purge is
// false the registry record is kept with State=StateDeleted (so `anvil
// purge` can remove it later, matching Multipass's delete/purge two-step);
// when purge is true the record is removed immediately.
func (m *Manager) Delete(ctx context.Context, names []string, purge bool) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			return err
		}
		if err := b.Delete(ctx, spec); err != nil {
			return fmt.Errorf("instance: deleting %s: %w", spec.Name, err)
		}
		if purge {
			if err := m.registry.DeleteByID(spec.ID); err != nil {
				return err
			}
			continue
		}
		spec.State = StateDeleted
		if err := m.registry.PutInstance(spec); err != nil {
			return err
		}
	}
	return nil
}

// Purge permanently removes records already in StateDeleted. An empty
// names list purges every such record.
func (m *Manager) Purge(names []string) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.State != StateDeleted {
			continue
		}
		if err := m.registry.DeleteByID(spec.ID); err != nil {
			return err
		}
	}
	return nil
}
