// Package ephemeral runs commands in throwaway clones restored from warm snapshots:
// `anvil launch --ephemeral`, `anvil matrix` and `anvil warm`.
package ephemeral

import (
	"context"
	"errors"
	"io"
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
}

// Engine is the hypervisor side: vm.FCBackend on Linux.
type Engine interface {
	// WarmSpec resolves p, failing when this host can't build warm snapshots.
	WarmSpec(p WarmParams) (WarmSpec, error)
	// BuildWarm boots a VM for s, waits for it to settle and snapshots it into dir.
	BuildWarm(ctx context.Context, s WarmSpec, dir string, progress func(string)) error
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
}
