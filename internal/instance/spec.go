// Package instance defines anvil's core domain model: the Spec/State that
// both the VM and container backends implement against, decoupled from the
// gRPC wire types (api/gen/anvil/v1) via an explicit conversion layer so
// storage format and API compatibility can evolve independently.
package instance

import "time"

// Kind distinguishes the two backend types anvil manages. A Spec has exactly
// one of VM or Container populated, matching its Kind.
type Kind string

const (
	KindVM        Kind = "vm"
	KindContainer Kind = "container"
)

// State is the lifecycle state of an instance, re-derived from the OS at
// daemon startup rather than blindly trusted from the registry — see
// internal/instance/manager.go's reconciliation pass.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateDeleting State = "deleting"
	StateDeleted  State = "deleted"
	StateError    State = "error"
)

// IntentLabel is the well-known Labels key used to tag an instance as a
// member of an intent group; the value is the owning Intent's ID.
const IntentLabel = "intent"

// RoleLabel is the well-known Labels key carrying the member's role within
// its intent (e.g. "web", "db"), set alongside IntentLabel.
const RoleLabel = "role"

type Spec struct {
	ID        string
	Name      string
	Kind      Kind
	State     State
	CreatedAt time.Time
	Labels    map[string]string

	VM        *VMSpec
	Container *ContainerSpec
}

type VMSpec struct {
	ImageRef  string
	Arch      string
	CPUs      int
	MemoryMiB int64
	DiskGiB   int64

	// Exactly one of CloudInitUserData or CloudInitName is normally set:
	// the former for an ad hoc/scripted --cloud-init file, the latter for
	// a reference into the saved cloud-init library (see internal/cloudinit,
	// added in M2).
	CloudInitUserData string
	CloudInitName     string

	NetworkMode   string // "slirp" | "bridge"
	SSHPublicKeys []string

	DiskPath    string
	SeedISOPath string

	// Populated by the VM backend, not by the launch request: SSHPort by
	// Create/Start (a freshly allocated SLIRP host-forward port — it can
	// change across a stop/start cycle), DefaultUser by Create (from the
	// image catalog's own DefaultUser). Both exist so `anvil shell`/`exec`/
	// `transfer` know how to reach a running instance without the user
	// having to grep `ps aux` for the forwarded port.
	SSHPort     int
	DefaultUser string

	// Mounts is only ever changed by `anvil mount`/`umount` (internal/vm.
	// Backend.Mount/Umount), never at launch time. NextMountIndex is a
	// monotonic counter for generating each mount's 9p tag ("mount0",
	// "mount1", ...) — it only ever increments, even across umounts, so a
	// tag is never reused within one instance's lifetime.
	//
	// Generation increments every time the cloud-init seed is rebuilt after
	// creation (i.e. on every Mount/Umount) and feeds into the seed's
	// instance-id (see internal/vm.Backend.buildSeed). This is deliberate:
	// cloud-init only guarantees re-running a "per-instance" module when it
	// sees what looks like a new instance-id, and there wasn't enough
	// confidence about the "mounts" module's actual default frequency to
	// rely on a plain reboot picking up a newly added mount otherwise — the
	// generation bump sidesteps needing to be right about that.
	Mounts         []Mount
	NextMountIndex int
	Generation     int
}

// Mount is one host directory shared into the guest over 9p.
type Mount struct {
	HostPath  string
	GuestPath string
	Tag       string
	ReadOnly  bool
}

type ContainerSpec struct {
	ImageRef    string
	Env         map[string]string
	Entrypoint  []string
	Cmd         []string
	Volumes     []VolumeMount
	Ports       []PortMapping
	NetworkMode string
}

type VolumeMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

type PortMapping struct {
	HostPort  int
	GuestPort int
	Protocol  string // "tcp" | "udp"
}
