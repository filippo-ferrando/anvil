package instance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oklog/ulid/v2"
)

// Manager orchestrates a Registry and the per-Kind Backend implementations.
type Manager struct {
	registry Registry
	backends map[Kind]Backend
}

func NewManager(registry Registry, backends map[Kind]Backend) *Manager {
	return &Manager{registry: registry, backends: backends}
}

// Reconcile re-derives the live state of every running/starting instance from its backend.
// Call once at daemon startup, before serving requests.
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

// LaunchParams is Manager.Launch's input.
type LaunchParams struct {
	Name      string
	Kind      Kind
	VM        *VMSpec
	Container *ContainerSpec
	NoStart   bool

	// Labels are stored on the instance verbatim.
	Labels map[string]string

	// IntentName/Role route the launch through internal/intent.Manager instead, when set.
	IntentName string
	Role       string

	// PinnedNetwork/PinnedStaticIP reuse an exact subnet/address instead of
	// auto-allocating, used when relaunching a migrated intent member.
	PinnedNetwork  *PinnedNetwork
	PinnedStaticIP string
}

// PinnedNetwork is an intent's exact subnet/gateway/docker-IP-range.
type PinnedNetwork struct {
	Subnet        string
	Gateway       string
	DockerIPRange string
}

// LaunchEvent is one step of Launch's progress callback: exactly one of
// Status, Instance, or Err is set per call.
type LaunchEvent struct {
	Status   string
	Instance *Spec
	Err      error
}

// Launch provisions (and, unless params.NoStart, starts) a new instance, reporting progress.
func (m *Manager) Launch(ctx context.Context, params LaunchParams, progress func(LaunchEvent)) error {
	b, err := m.backendFor(params.Kind)
	if err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}

	if _, err := m.registry.GetByName(params.Name); err == nil {
		err := fmt.Errorf("instance: name %q is already in use", params.Name)
		progress(LaunchEvent{Err: err})
		return err
	} else if !errors.Is(err, ErrNotFound) {
		progress(LaunchEvent{Err: err})
		return err
	}

	labels := make(map[string]string, len(params.Labels))
	for k, v := range params.Labels {
		labels[k] = v
	}

	spec := &Spec{
		ID:        ulid.Make().String(),
		Name:      params.Name,
		Kind:      params.Kind,
		State:     StateStarting,
		CreatedAt: time.Now(),
		Labels:    labels,
		VM:        params.VM,
		Container: params.Container,
	}

	progress(LaunchEvent{Status: "provisioning"})
	if err := b.Create(ctx, spec, func(status string) { progress(LaunchEvent{Status: status}) }); err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}

	if params.NoStart {
		spec.State = StateStopped
	} else {
		progress(LaunchEvent{Status: "starting"})
		if err := b.Start(ctx, spec); err != nil {
			// Roll back what Create already provisioned so nothing is leaked untracked.
			if delErr := b.Delete(ctx, spec); delErr != nil {
				log.Printf("instance: rolling back failed start of %s (%s): %v", spec.Name, spec.ID, delErr)
			}
			progress(LaunchEvent{Err: err})
			return err
		}
		spec.State = StateRunning
	}

	if err := m.registry.PutInstance(spec); err != nil {
		// Tear down the just-provisioned resource rather than leave it live and untracked.
		if delErr := b.Delete(ctx, spec); delErr != nil {
			log.Printf("instance: rolling back %s (%s) after registry write failure: %v", spec.Name, spec.ID, delErr)
		}
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

// GetByID looks up a single instance by ID.
func (m *Manager) GetByID(id string) (*Spec, error) {
	return m.registry.GetByID(id)
}

// Stats returns name's current resource-usage snapshot.
func (m *Manager) Stats(ctx context.Context, name string) (Stats, error) {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return Stats{}, err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return Stats{}, err
	}
	sp, ok := b.(StatsProvider)
	if !ok {
		return Stats{}, fmt.Errorf("instance: %s instances don't support stats", spec.Kind)
	}
	return sp.Stats(ctx, spec)
}

// Logs streams name's log output to send.
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

// Mount shares hostPath into name's guest at guestPath.
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

// AddPort adds a host-to-guest port forward to name, live if it's running.
func (m *Manager) AddPort(ctx context.Context, name string, port PortMapping) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	pf, ok := b.(PortForwarder)
	if !ok {
		return fmt.Errorf("instance: %s instances don't support port forwarding changes", spec.Kind)
	}
	if err := pf.AddPort(ctx, spec, port); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

// RemovePort removes a port forward previously added with AddPort,
// identified by hostPort/protocol.
func (m *Manager) RemovePort(ctx context.Context, name string, hostPort int, protocol string) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	pf, ok := b.(PortForwarder)
	if !ok {
		return fmt.Errorf("instance: %s instances don't support port forwarding changes", spec.Kind)
	}
	if err := pf.RemovePort(ctx, spec, hostPort, protocol); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

// snapshotter resolves name to its spec and Snapshotter backend, for
// CreateSnapshot/RestoreSnapshot/DeleteSnapshot/ListSnapshots.
func (m *Manager) snapshotter(name string) (*Spec, Snapshotter, error) {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return nil, nil, err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return nil, nil, err
	}
	sn, ok := b.(Snapshotter)
	if !ok {
		return nil, nil, fmt.Errorf("instance: %s instances don't support snapshots", spec.Kind)
	}
	return spec, sn, nil
}

// CreateSnapshot creates a new named snapshot of name's disk.
func (m *Manager) CreateSnapshot(ctx context.Context, name, snapshotName string) error {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return err
	}
	if err := sn.CreateSnapshot(ctx, spec, snapshotName); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

// RestoreSnapshot resets name's disk back to a previously created snapshot.
func (m *Manager) RestoreSnapshot(ctx context.Context, name, snapshotName string) error {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return err
	}
	if err := sn.RestoreSnapshot(ctx, spec, snapshotName); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

// DeleteSnapshot removes a previously created snapshot.
func (m *Manager) DeleteSnapshot(ctx context.Context, name, snapshotName string) error {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return err
	}
	if err := sn.DeleteSnapshot(ctx, spec, snapshotName); err != nil {
		return err
	}
	return m.registry.PutInstance(spec)
}

// ListSnapshots returns every snapshot currently recorded on name's disk.
func (m *Manager) ListSnapshots(ctx context.Context, name string) ([]Snapshot, error) {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return nil, err
	}
	return sn.ListSnapshots(ctx, spec)
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

// Delete tears down each named instance's backend resources, keeping the
// registry record (as StateDeleted) unless purge is true. Every name is
// attempted even if an earlier one fails.
func (m *Manager) Delete(ctx context.Context, names []string, purge bool) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	var errs []error
	for _, spec := range specs {
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := b.Delete(ctx, spec); err != nil {
			errs = append(errs, fmt.Errorf("instance: deleting %s: %w", spec.Name, err))
			continue
		}
		if purge {
			if err := m.registry.DeleteByID(spec.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		spec.State = StateDeleted
		if err := m.registry.PutInstance(spec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Purge permanently removes records already in StateDeleted; an empty names list purges all of them.
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
