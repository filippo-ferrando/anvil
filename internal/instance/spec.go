// Package instance defines anvil's core domain model shared by the VM and container backends.
package instance

import (
	"fmt"
	"strings"
	"time"
)

// Kind distinguishes the two backend types anvil manages.
type Kind string

const (
	KindVM        Kind = "vm"
	KindContainer Kind = "container"
)

// State is an instance's lifecycle state.
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

// IntentLabel is the Labels key tagging an instance's owning intent ID.
const IntentLabel = "intent"

// RoleLabel is the Labels key carrying a member's role within its intent.
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

	// Exactly one of CloudInitUserData (ad hoc) or CloudInitName (saved library) is set.
	CloudInitUserData string
	CloudInitName     string

	NetworkMode   string // "slirp" | "bridge"
	SSHPublicKeys []string

	// Ports to publish from the host into the guest; only meaningful in SLIRP mode.
	Ports []PortMapping

	DiskPath    string
	SeedISOPath string

	// SSHPort/DefaultUser are populated by the backend at Create/Start, not by the launch request.
	SSHPort     int
	DefaultUser string

	// Mounts/NextMountIndex/Generation are only changed by `anvil mount`/`umount`.
	Mounts         []Mount
	NextMountIndex int
	Generation     int

	// BridgeInterface/StaticIP/Gateway are populated by internal/intent.Manager
	// when this VM joins an intent; meaningless otherwise.
	BridgeInterface string
	StaticIP        string // CIDR, e.g. "10.55.201.4/24"
	Gateway         string

	// ExtraHosts is every other intent member's role -> IP known at launch time.
	ExtraHosts map[string]string

	// SourceDiskPath, if set, makes Create adopt this disk directly instead
	// of resolving the image catalog (used by migration).
	SourceDiskPath string
}

// Mount is one host directory shared into the guest over 9p.
type Mount struct {
	HostPath  string
	GuestPath string
	Tag       string
	ReadOnly  bool
}

// ContainerEngine selects which container runtime a ContainerSpec runs on.
type ContainerEngine string

const (
	ContainerEngineDocker ContainerEngine = "docker"
	ContainerEnginePodman ContainerEngine = "podman"
)

type ContainerSpec struct {
	ImageRef    string
	Env         map[string]string
	Entrypoint  []string
	Cmd         []string
	Volumes     []VolumeMount
	Ports       []PortMapping
	NetworkMode string
	Engine      ContainerEngine

	// ContainerID is the underlying engine's own ID, populated by Create.
	ContainerID string

	// NetworkAlias/ExtraHosts are populated by internal/intent.Manager for an
	// intent member; meaningless otherwise.
	NetworkAlias string
	ExtraHosts   map[string]string
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

// FormatPorts renders ports as "8080:80/tcp, 2222:22/tcp", used to list
// an instance's currently exposed ports in a "no such port forward" error.
func FormatPorts(ports []PortMapping) string {
	if len(ports) == 0 {
		return "none"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		parts[i] = fmt.Sprintf("%d:%d/%s", p.HostPort, p.GuestPort, proto)
	}
	return strings.Join(parts, ", ")
}

// Snapshot is one point-in-time internal QCOW2 snapshot of a VM's disk.
type Snapshot struct {
	Name      string
	CreatedAt time.Time

	// HasVMState is true for a live QMP snapshot (embeds RAM, not just disk)
	// and false for an offline one. Restore only ever resets the disk, so a restart after it always boots fresh regardless of this flag.
	HasVMState bool
}
