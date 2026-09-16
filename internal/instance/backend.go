package instance

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound means a registry lookup found no matching record.
var ErrNotFound = errors.New("not found")

// Backend dispatches lifecycle operations for one instance Kind (VM or container).
type Backend interface {
	// Create provisions spec but does not start it.
	Create(ctx context.Context, spec *Spec, progress func(status string)) error

	Start(ctx context.Context, spec *Spec) error

	// Stop requests a shutdown; force skips any graceful attempt, timeout bounds it.
	Stop(ctx context.Context, spec *Spec, force bool, timeout time.Duration) error

	// Delete removes spec's backing resources (disk files, container).
	Delete(ctx context.Context, spec *Spec) error

	// Status reports the backend's live view of spec's current state.
	Status(ctx context.Context, spec *Spec) (State, error)

	// Logs streams spec's log output to send, following new output if requested.
	Logs(ctx context.Context, spec *Spec, follow bool, tailLines int, send func(chunk []byte) error) error
}

// Mounter is implemented by a Backend that can share a host directory into a running instance.
type Mounter interface {
	Mount(ctx context.Context, spec *Spec, hostPath, guestPath string, readOnly bool) error
	Umount(ctx context.Context, spec *Spec, guestPath string) error
}

// PortForwarder is implemented by a Backend that can add/remove a
// host-to-guest port forward on an existing instance. A VM backend can
// usually do this live, without a restart; a container engine backend
// (Docker has no live port-binding mutation) recreates the container
// instead — either way, the caller doesn't need to relaunch the instance.
type PortForwarder interface {
	AddPort(ctx context.Context, spec *Spec, port PortMapping) error

	// RemovePort identifies the mapping to remove by hostPort/protocol
	// ("tcp"/"udp"), same as the one originally passed to AddPort.
	RemovePort(ctx context.Context, spec *Spec, hostPort int, protocol string) error
}

// Reconciler is implemented by a Backend that can re-derive an instance's live state after a restart.
type Reconciler interface {
	Reconcile(ctx context.Context, spec *Spec) (State, error)
}

// Stats is a point-in-time resource-usage reading for one running instance.
type Stats struct {
	CPUPercent float64 // normalized to the instance's own CPU allocation

	MemUsedBytes  int64
	MemLimitBytes int64

	// Disk space (VM only); DiskTotalBytes of 0 means space usage isn't available.
	DiskUsedBytes  int64
	DiskTotalBytes int64

	// Disk I/O rate (container only), a short live average.
	DiskReadBytesPerSec  float64
	DiskWriteBytesPerSec float64

	// Network throughput, a short live average. Unavailable for a SLIRP-mode VM.
	NetAvailable     bool
	NetRxBytesPerSec float64
	NetTxBytesPerSec float64

	UptimeSeconds int64
	Address       string
}

// StatsProvider is implemented by a Backend that can report live resource usage.
type StatsProvider interface {
	Stats(ctx context.Context, spec *Spec) (Stats, error)
}

// Registry is the persistence contract Manager needs.
type Registry interface {
	PutInstance(spec *Spec) error
	GetByID(id string) (*Spec, error)
	GetByName(name string) (*Spec, error)
	List(kindFilter Kind) ([]*Spec, error)
	DeleteByID(id string) error
}
