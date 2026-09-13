package instance

import (
	"context"
	"time"
)

// Backend is implemented by each concrete backend (internal/vm for
// instance.KindVM, internal/container/podman for instance.KindContainer,
// M3) so Manager can dispatch on a Spec's Kind without knowing which
// concrete backend it's talking to — this is what makes
// list/start/stop/delete/info behave uniformly across VMs and containers.
//
// This interface lives in package instance itself, alongside Spec, rather
// than in a separate internal/backend package: a separate package would
// import instance for Spec/Kind/State, and Manager (below, same package as
// Spec) would need to import that package back for the Backend type —
// a cycle. Concrete backends (internal/vm, etc.) importing instance for
// this interface is a one-way dependency and doesn't have that problem.
type Backend interface {
	// Create provisions spec (disk/seed for a VM, container config for a
	// container) but does not start it — Create and Start are split so
	// LaunchRequest.NoStart can provision without starting. Implementations
	// may populate additional fields on spec (e.g. VMSpec.DiskPath) that
	// the caller is expected to persist afterward.
	Create(ctx context.Context, spec *Spec) error

	Start(ctx context.Context, spec *Spec) error

	// Stop requests a shutdown; force skips any graceful attempt. timeout
	// bounds how long a graceful stop is given before the backend may
	// escalate on its own (see internal/vm/qemu.Process.Stop).
	Stop(ctx context.Context, spec *Spec, force bool, timeout time.Duration) error

	// Delete removes spec's backing resources (disk files, container).
	// The caller is responsible for removing spec from the registry —
	// Delete only tears down backend-owned state.
	Delete(ctx context.Context, spec *Spec) error

	// Status reports the backend's live view of spec's state, used to
	// correct registry drift (e.g. a VM killed out-of-band) rather than
	// trusting whatever was last persisted.
	Status(ctx context.Context, spec *Spec) (State, error)

	// Logs streams spec's log output to send, one chunk at a time. For a
	// VM this is boot/console output (see internal/vm.Backend.Logs), not
	// application logs from inside the guest — see the plan's "VM vs
	// container" CLI notes for why those are different concepts. If follow
	// is true, Logs keeps sending new output as it arrives until ctx is
	// canceled; tailLines of 0 means "from the beginning".
	Logs(ctx context.Context, spec *Spec, follow bool, tailLines int, send func(chunk []byte) error) error
}

// Mounter is optionally implemented by a Backend that supports sharing a
// host directory into a running instance — see internal/vm.Backend's
// Mount/Umount for the concrete (9p-based) VM implementation. Not every
// backend kind necessarily has an equivalent concept (a container's
// volumes are configured at creation time via ContainerSpec.Volumes
// instead), so Manager.Mount/Umount return a clear error for a Backend
// that doesn't implement this rather than assuming every kind supports it.
type Mounter interface {
	Mount(ctx context.Context, spec *Spec, hostPath, guestPath string, readOnly bool) error
	Umount(ctx context.Context, spec *Spec, guestPath string) error
}

// Reconciler is optionally implemented by a Backend that can re-derive an
// instance's live state after a daemon restart, instead of trusting
// whatever the registry last recorded — see internal/vm.Backend.Reconcile
// for the concrete VM implementation (pid liveness + cmdline match + QMP
// reachability). A backend that doesn't need this (e.g. one that always
// queries live state fresh with nothing to lose on restart) just doesn't
// implement it; Manager.Reconcile skips backends that don't.
type Reconciler interface {
	Reconcile(ctx context.Context, spec *Spec) (State, error)
}

// Registry is the persistence contract Manager needs. It lives here (as
// an interface, not a concrete type) for the same reason Backend does:
// internal/store imports this package for Spec/Kind/State, so this
// package can't import internal/store back without a cycle. *store.Store
// (internal/store/bolt.go) already satisfies this interface structurally
// — Manager is constructed with one injected via NewManager, wiring
// happens in internal/daemon, which is free to import both packages.
type Registry interface {
	PutInstance(spec *Spec) error
	GetByID(id string) (*Spec, error)
	GetByName(name string) (*Spec, error)
	List(kindFilter Kind) ([]*Spec, error)
	DeleteByID(id string) error
}
