// Package instance defines anvil's core domain model shared by the VM and container backends.
package instance

import (
	"fmt"
	"strconv"
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

	// Guest is live, never persisted: what the guest agent last reported (VM only, nil if unknown).
	Guest *GuestInfo

	// Autostart starts the instance when anvild starts, unless UserStopped.
	Autostart     bool
	RestartPolicy RestartPolicy
	// UserStopped is set by an explicit stop and cleared by a start, so autostart and
	// the restart policy leave an instance someone stopped on purpose alone.
	UserStopped bool
}

// Restart modes for RestartPolicy.Mode.
const (
	RestartNo        = "no"
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"
)

// RestartPolicy says what to do when an instance stops without anvil asking.
type RestartPolicy struct {
	Mode       string // RestartNo (also when empty), RestartOnFailure or RestartAlways
	MaxRetries int    // on-failure only: restarts in a row before giving up; 0 = no limit
}

// ParseRestartPolicy reads "no", "always", "on-failure" or "on-failure:N".
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	mode, retries, hasRetries := strings.Cut(strings.TrimSpace(s), ":")
	switch mode {
	case "", RestartNo, RestartAlways:
		if hasRetries {
			return RestartPolicy{}, fmt.Errorf("instance: only on-failure takes a retry count, got %q", s)
		}
		if mode == "" {
			mode = RestartNo
		}
		return RestartPolicy{Mode: mode}, nil
	case RestartOnFailure:
		p := RestartPolicy{Mode: mode}
		if hasRetries {
			n, err := strconv.Atoi(retries)
			if err != nil || n < 0 {
				return RestartPolicy{}, fmt.Errorf("instance: invalid retry count in %q", s)
			}
			p.MaxRetries = n
		}
		return p, nil
	default:
		return RestartPolicy{}, fmt.Errorf("instance: unknown restart policy %q (want no, on-failure[:N] or always)", s)
	}
}

// String is the inverse of ParseRestartPolicy.
func (p RestartPolicy) String() string {
	switch {
	case p.Mode == "":
		return RestartNo
	case p.Mode == RestartOnFailure && p.MaxRetries > 0:
		return fmt.Sprintf("%s:%d", p.Mode, p.MaxRetries)
	default:
		return p.Mode
	}
}

// SnapshotSchedule takes a live snapshot of a running VM every Every, keeping the newest Keep.
type SnapshotSchedule struct {
	Every   time.Duration
	Keep    int
	LastRun time.Time
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

	// MountFS is "virtiofs" once the guest's fstab was written for virtiofs; empty for
	// VMs whose mounts date from 9p, which get their fstab rewritten on the next start.
	MountFS string

	// BridgeInterface/StaticIP/Gateway are populated by internal/intent.Manager
	// when this VM joins an intent; meaningless otherwise.
	BridgeInterface string
	StaticIP        string // CIDR, e.g. "10.55.201.4/24"
	Gateway         string

	// ExtraHosts is every other intent member's role -> IP known at launch time.
	ExtraHosts map[string]string

	// DNSServers/DNSSearch point the guest at its intent's DNS server, so
	// members added later still resolve by name.
	DNSServers []string
	DNSSearch  []string

	// SourceDiskPath, if set, makes Create adopt this disk directly instead
	// of resolving the image catalog (used by migration).
	SourceDiskPath string

	// SourceDiskBaseSHA256, if set, marks SourceDiskPath as a delta on top of
	// the base image with this checksum, to be rebased onto the local copy.
	SourceDiskBaseSHA256 string

	// SourceBaseImagePath, if set, is that base image itself, sent along because
	// this host didn't have it. Adopted before the rebase.
	SourceBaseImagePath string

	// NoGuestAgent skips installing qemu-guest-agent through cloud-init.
	NoGuestAgent bool

	// SnapshotSchedule is nil when no scheduled snapshots are set up.
	SnapshotSchedule *SnapshotSchedule
}

// Mount is one host directory shared into the guest over virtiofs.
type Mount struct {
	HostPath  string
	GuestPath string
	Tag       string
	ReadOnly  bool

	// SourceDataPath, if set, is where this folder's contents were staged by a
	// migration or an import. They are moved into place before the instance is created.
	SourceDataPath string
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
	DNSServers   []string
	DNSSearch    []string
}

type VolumeMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool

	// SourceDataPath works the same as Mount.SourceDataPath.
	SourceDataPath string
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

// ParsePortMapping reads "<host-port>:<guest-port>[/tcp|udp]"; the protocol defaults to tcp.
func ParsePortMapping(s string) (PortMapping, error) {
	spec, proto, hasProto := strings.Cut(s, "/")
	if !hasProto {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return PortMapping{}, fmt.Errorf("port %q: protocol must be tcp or udp", s)
	}
	hostStr, guestStr, ok := strings.Cut(spec, ":")
	if !ok {
		return PortMapping{}, fmt.Errorf(`port %q must be "<host-port>:<guest-port>[/tcp|udp]"`, s)
	}
	host, err1 := strconv.Atoi(hostStr)
	guest, err2 := strconv.Atoi(guestStr)
	if err1 != nil || err2 != nil || host < 1 || host > 65535 || guest < 1 || guest > 65535 {
		return PortMapping{}, fmt.Errorf("port %q: ports must be numbers from 1 to 65535", s)
	}
	return PortMapping{HostPort: host, GuestPort: guest, Protocol: proto}, nil
}

// ParseVolumeMount reads "<host-path>:<container-path>[:ro]".
func ParseVolumeMount(s string) (VolumeMount, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return VolumeMount{}, fmt.Errorf(`volume %q must be "<host-path>:<container-path>[:ro]"`, s)
	}
	if len(parts) == 3 && parts[2] != "ro" {
		return VolumeMount{}, fmt.Errorf(`volume %q: third part must be "ro"`, s)
	}
	return VolumeMount{HostPath: parts[0], ContainerPath: parts[1], ReadOnly: len(parts) == 3}, nil
}
