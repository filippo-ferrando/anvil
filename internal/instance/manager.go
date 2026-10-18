package instance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Manager orchestrates a Registry and the per-Kind Backend implementations.
type Manager struct {
	registry Registry
	backends map[Kind]Backend
	events   *broadcaster[Event]
	restarts restartTracker

	// History persists the event log behind `anvil events`, taken from the registry when it
	// implements HistoryStore; nil keeps events live only.
	History HistoryStore
	history broadcaster[HistoryEvent]
}

// NewManager returns a Manager dispatching to one Backend per Kind.
func NewManager(registry Registry, backends map[Kind]Backend) *Manager {
	events := &broadcaster[Event]{}
	m := &Manager{backends: backends, events: events}
	m.History, _ = registry.(HistoryStore) // before the hooks below can record anything
	m.registry = notifyingRegistry{Registry: registry, events: events, decorate: m.withGuest}
	m.hookGuestChanges()
	m.hookExits()
	return m
}

// Reconcile re-derives the live state of every running/starting instance from its backend,
// and removes records left deleted by older versions. Call once at startup, before serving requests.
func (m *Manager) Reconcile(ctx context.Context) error {
	specs, err := m.registry.List("")
	if err != nil {
		return fmt.Errorf("instance: listing instances to reconcile: %w", err)
	}
	for _, spec := range specs {
		if spec.State == StateDeleted {
			// Left by versions whose delete kept the record (and, briefly, the disk).
			log.Printf("instance: removing %s, left deleted by an older version", spec.Name)
			if err := m.Delete(ctx, []string{spec.Name}); err != nil {
				log.Printf("instance: %v", err)
			}
			continue
		}
		if spec.State != StateRunning && spec.State != StateStarting {
			continue
		}
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			continue // no backend registered for this kind (yet); leave state as-is
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
		if newState != StateRunning {
			m.record(ctx, spec, "exit", "found "+string(newState)+" when anvild started", nil)
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

	Autostart     bool
	RestartPolicy RestartPolicy
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

		Autostart:     params.Autostart,
		RestartPolicy: params.RestartPolicy,
		GUI:           true,
	}

	// fail records a failed launch; the instance never existed, so the event has no ID.
	fail := func(err error) error {
		m.record(ctx, &Spec{Name: spec.Name, Kind: spec.Kind}, "launch", imageOf(spec), err)
		progress(LaunchEvent{Err: err})
		return err
	}

	progress(LaunchEvent{Status: "provisioning"})
	if err := b.Create(ctx, spec, func(status string) { progress(LaunchEvent{Status: status}) }); err != nil {
		return fail(err)
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
			return fail(err)
		}
		spec.State = StateRunning
	}

	if err := m.registry.PutInstance(spec); err != nil {
		// Tear down the just-provisioned resource rather than leave it live and untracked.
		if delErr := b.Delete(ctx, spec); delErr != nil {
			log.Printf("instance: rolling back %s (%s) after registry write failure: %v", spec.Name, spec.ID, delErr)
		}
		return fail(err)
	}

	msg := imageOf(spec)
	if params.NoStart {
		msg += ", not started"
	}
	m.record(ctx, spec, "launch", msg, nil)
	progress(LaunchEvent{Instance: spec})
	return nil
}

// imageOf returns the image an instance was created from, for event messages.
func imageOf(spec *Spec) string {
	if spec.VM != nil {
		return spec.VM.ImageRef
	}
	if spec.Container != nil {
		return spec.Container.ImageRef
	}
	return ""
}

// ForkParams is Manager.Fork's input.
type ForkParams struct {
	Source  string // source instance's name
	NewName string
	Start   bool // start the forked instance immediately after creating it
}

// Fork creates a new instance named params.NewName whose disk starts as a
// copy of params.Source's disk, carrying over its VM config but not its network identity: the fork always starts as a standalone SLIRP instance.
func (m *Manager) Fork(ctx context.Context, params ForkParams, progress func(LaunchEvent)) error {
	source, err := m.registry.GetByName(params.Source)
	if err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}

	b, err := m.backendFor(source.Kind)
	if err != nil {
		progress(LaunchEvent{Err: err})
		return err
	}
	forker, ok := b.(Forker)
	if !ok {
		err := fmt.Errorf("instance: %s instances don't support fork", source.Kind)
		progress(LaunchEvent{Err: err})
		return err
	}

	if _, err := m.registry.GetByName(params.NewName); err == nil {
		err := fmt.Errorf("instance: name %q is already in use", params.NewName)
		progress(LaunchEvent{Err: err})
		return err
	} else if !errors.Is(err, ErrNotFound) {
		progress(LaunchEvent{Err: err})
		return err
	}

	labels := make(map[string]string, len(source.Labels))
	for k, v := range source.Labels {
		labels[k] = v
	}

	dest := &Spec{
		ID:        ulid.Make().String(),
		Name:      params.NewName,
		Kind:      source.Kind,
		State:     StateStarting,
		CreatedAt: time.Now(),
		Labels:    labels,
		VM:        cloneVMSpecForFork(source.VM),
		GUI:       true,
	}

	msg := "forked from " + source.Name
	fail := func(err error) error {
		m.record(ctx, &Spec{Name: dest.Name, Kind: dest.Kind}, "fork", msg, err)
		progress(LaunchEvent{Err: err})
		return err
	}

	progress(LaunchEvent{Status: "forking disk"})
	if err := forker.Fork(ctx, source, dest, func(status string) { progress(LaunchEvent{Status: status}) }); err != nil {
		return fail(err)
	}

	if params.Start {
		progress(LaunchEvent{Status: "starting"})
		if err := b.Start(ctx, dest); err != nil {
			if delErr := b.Delete(ctx, dest); delErr != nil {
				log.Printf("instance: rolling back failed start of fork %s (%s): %v", dest.Name, dest.ID, delErr)
			}
			return fail(err)
		}
		dest.State = StateRunning
	} else {
		dest.State = StateStopped
	}

	if err := m.registry.PutInstance(dest); err != nil {
		if delErr := b.Delete(ctx, dest); delErr != nil {
			log.Printf("instance: rolling back fork %s (%s) after registry write failure: %v", dest.Name, dest.ID, delErr)
		}
		return fail(err)
	}

	m.record(ctx, dest, "fork", msg, nil)
	progress(LaunchEvent{Instance: dest})
	return nil
}

// cloneVMSpecForFork deep-copies v's configuration for a new forked
// instance, clearing fields the backend/network populate fresh (disk/seed paths, SSH port, bridge/intent addressing).
func cloneVMSpecForFork(v *VMSpec) *VMSpec {
	clone := *v
	clone.NetworkMode = "slirp"
	clone.SSHPublicKeys = append([]string(nil), v.SSHPublicKeys...)
	clone.Ports = append([]PortMapping(nil), v.Ports...)
	clone.Mounts = append([]Mount(nil), v.Mounts...)
	clone.DiskPath = ""
	clone.SeedISOPath = ""
	clone.SSHPort = 0
	clone.BridgeInterface = ""
	clone.StaticIP = ""
	clone.Gateway = ""
	clone.ExtraHosts = nil
	clone.DNSServers = nil
	clone.DNSSearch = nil
	clone.SourceDiskPath = ""
	clone.SourceDiskBaseSHA256 = ""
	clone.MACAddress = "" // derived again from the fork's own ID
	return &clone
}

// List returns every instance of kindFilter, or all of them when it is empty,
// with live guest detail filled in where a backend reports it.
func (m *Manager) List(kindFilter Kind) ([]*Spec, error) {
	specs, err := m.registry.List(kindFilter)
	return m.withGuests(specs), err
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

// Info returns the named instances, with live guest detail filled in.
func (m *Manager) Info(names []string) ([]*Spec, error) {
	specs, err := m.resolve(names)
	return m.withGuests(specs), err
}

// GetByID looks up a single instance by ID.
func (m *Manager) GetByID(id string) (*Spec, error) {
	spec, err := m.registry.GetByID(id)
	return m.withGuest(spec), err
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

// StreamStats sends name's stats every interval. It returns nil once the instance is no
// longer running or ctx ends, and an error when sampling or send fails.
func (m *Manager) StreamStats(ctx context.Context, name string, interval time.Duration, send func(Stats) error) error {
	for {
		st, err := m.Stats(ctx, name)
		if ctx.Err() != nil {
			return nil
		}
		if spec, gErr := m.registry.GetByName(name); gErr != nil {
			return gErr
		} else if spec.State != StateRunning {
			return nil // checked after sampling too, so a stop mid-sample is a clean end
		}
		if err != nil {
			return err
		}
		if err := send(st); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
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
	msg := hostPath + " -> " + guestPath
	if readOnly {
		msg += " (ro)"
	}
	err = mounter.Mount(ctx, spec, hostPath, guestPath, readOnly)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "mount", msg, err)
	return err
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
	err = mounter.Umount(ctx, spec, guestPath)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "umount", guestPath, err)
	return err
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
	err = pf.AddPort(ctx, spec, port)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "port-add", fmt.Sprintf("%d -> %d/%s", port.HostPort, port.GuestPort, Protocol(port.Protocol)), err)
	return err
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
	err = pf.RemovePort(ctx, spec, hostPort, protocol)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "port-remove", fmt.Sprintf("%d/%s", hostPort, Protocol(protocol)), err)
	return err
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
	err = sn.CreateSnapshot(ctx, spec, snapshotName)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "snapshot-create", snapshotName, err)
	return err
}

// RestoreSnapshot resets name's disk back to a previously created snapshot.
func (m *Manager) RestoreSnapshot(ctx context.Context, name, snapshotName string) error {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return err
	}
	err = sn.RestoreSnapshot(ctx, spec, snapshotName)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "snapshot-restore", snapshotName, err)
	return err
}

// DeleteSnapshot removes a previously created snapshot.
func (m *Manager) DeleteSnapshot(ctx context.Context, name, snapshotName string) error {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return err
	}
	err = sn.DeleteSnapshot(ctx, spec, snapshotName)
	if err == nil {
		err = m.registry.PutInstance(spec)
	}
	m.record(ctx, spec, "snapshot-delete", snapshotName, err)
	return err
}

// ListSnapshots returns every snapshot currently recorded on name's disk.
func (m *Manager) ListSnapshots(ctx context.Context, name string) ([]Snapshot, error) {
	spec, sn, err := m.snapshotter(name)
	if err != nil {
		return nil, err
	}
	return sn.ListSnapshots(ctx, spec)
}

// maxParallelOps caps how many instances Start/Stop drive at the same time.
const maxParallelOps = 4

// forEachParallel runs fn on every spec with at most maxParallelOps at once.
// Every spec is attempted even if an earlier one fails; errors are joined.
func forEachParallel(specs []*Spec, fn func(*Spec) error) error {
	errs := make([]error, len(specs))
	sem := make(chan struct{}, maxParallelOps)
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = fn(spec)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Start starts each named instance, at most maxParallelOps at a time, and
// tries every one even if an earlier instance fails.
func (m *Manager) Start(ctx context.Context, names []string) error {
	return m.start(ctx, names, "start")
}

// start is Start, recording each instance's event as action.
func (m *Manager) start(ctx context.Context, names []string, action string) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	return forEachParallel(specs, func(spec *Spec) error {
		err := m.startOne(ctx, spec)
		m.record(ctx, spec, action, "", err)
		return err
	})
}

func (m *Manager) startOne(ctx context.Context, spec *Spec) error {
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	if err := b.Start(ctx, spec); err != nil {
		return fmt.Errorf("instance: starting %s: %w", spec.Name, err)
	}
	spec.State = StateRunning
	spec.UserStopped = false
	m.restarts.started(spec.ID)
	return m.registry.PutInstance(spec)
}

// Stop stops each named instance, at most maxParallelOps at a time, and tries
// every one even if an earlier instance fails. A VM escalates from the guest
// agent to ACPI to a hard stop, all inside timeout; force skips to the end.
func (m *Manager) Stop(ctx context.Context, names []string, force bool, timeout time.Duration) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	msg := ""
	if force {
		msg = "forced"
	}
	return forEachParallel(specs, func(spec *Spec) error {
		err := m.stopOne(ctx, spec, force, timeout)
		m.record(ctx, spec, "stop", msg, err)
		return err
	})
}

func (m *Manager) stopOne(ctx context.Context, spec *Spec, force bool, timeout time.Duration) error {
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	m.restarts.cancel(spec.ID)
	if err := b.Stop(ctx, spec, force, timeout); err != nil {
		return fmt.Errorf("instance: stopping %s: %w", spec.Name, err)
	}
	spec.State = StateStopped
	spec.UserStopped = true
	return m.registry.PutInstance(spec)
}

// Delete tears down each named instance's backend resources (disk, container) and
// removes its record; every name is attempted even if an earlier one fails.
func (m *Manager) Delete(ctx context.Context, names []string) error {
	specs, err := m.resolve(names)
	if err != nil {
		return err
	}
	var errs []error
	for _, spec := range specs {
		err := m.deleteOne(ctx, spec)
		m.record(ctx, spec, "delete", "", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) deleteOne(ctx context.Context, spec *Spec) error {
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return err
	}
	m.restarts.cancel(spec.ID)
	if err := b.Delete(ctx, spec); err != nil {
		return fmt.Errorf("instance: deleting %s: %w", spec.Name, err)
	}
	return m.registry.DeleteByID(spec.ID)
}
