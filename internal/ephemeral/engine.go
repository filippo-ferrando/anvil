// Package ephemeral runs commands in throwaway clones restored from warm snapshots:
// `anvil launch --ephemeral`, `anvil matrix` and `anvil warm`.
package ephemeral

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// ErrUnsupported is returned on hosts that can't run ephemeral clones at all.
var ErrUnsupported = errors.New("ephemeral runs need Linux with Firecracker and its jailer")

// Defaults for a warm snapshot. The memory file is as large as the guest's RAM.
const (
	DefaultCPUs      = 2
	DefaultMemoryMiB = 512
)

// WarmParams asks for a warm snapshot of one distro with one machine size.
type WarmParams struct {
	Distro    string // catalog id or alias, as `anvil launch` takes it
	Arch      string // empty for the host's
	CPUs      int
	MemoryMiB int64
	// App is a package installed in the snapshot, with the GUI tools, for `anvil gui --ephemeral`.
	App string
	X11 bool // with App: also Xwayland
}

// WarmSpec is a resolved WarmParams, plus the key that names its snapshot: a hash of
// everything the snapshot depends on (image, size, cloud-init, firecracker, kernel, anvil-guest).
type WarmSpec struct {
	Key                string
	Distro             string // catalog id
	Arch               string
	CPUs               int
	MemoryMiB          int64
	DiskGiB            int64
	FirecrackerVersion string
	User               string // the image's default user, who runs GUI apps
	App                string
	X11                bool
}

// Dialer opens a new connection to a VM's anvil-guest.
type Dialer func(ctx context.Context) (io.ReadWriteCloser, error)

// Setup runs in a warm build VM once anvil-guest answers, before the snapshot is taken.
type Setup func(ctx context.Context, dial Dialer, progress func(string)) error

// Engine is the hypervisor side: vm.FCBackend on Linux.
type Engine interface {
	// WarmSpec resolves p, failing when this host can't build warm snapshots.
	WarmSpec(p WarmParams) (WarmSpec, error)
	// BuildWarm boots a VM for s, waits for it to settle, runs setup (when not nil) and
	// snapshots it into dir.
	BuildWarm(ctx context.Context, s WarmSpec, dir string, setup Setup, progress func(string)) error
	// RestoreClone starts clone id from the warm snapshot in dir.
	RestoreClone(ctx context.Context, id string, s WarmSpec, dir string) (Clone, error)
	// DeleteClone stops clone id and removes everything it holds.
	DeleteClone(ctx context.Context, id string) error
	// SweepClones removes clones an earlier anvild left behind.
	SweepClones(ctx context.Context) error
}

// Clone is a running clone, reached through anvil-guest.
type Clone interface {
	ID() string
	// Dial opens a new connection to the clone's anvil-guest.
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
	StartedAt() time.Time
	// Listen accepts the connections the guest opens to the host's vsock port.
	Listen(port uint32) (net.Listener, error)
}
