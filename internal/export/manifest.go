// Package export implements `anvil export`/`anvil import`: packaging a single instance or a whole
// intent into a portable tar.zst bundle, and relaunching one back from it.
package export

// Manifest is a bundle's manifest.json: everything needed to relaunch every
// member, plus archive-relative paths to their bulk data (disks, volumes).
type Manifest struct {
	// IntentName is empty for a standalone instance bundle.
	IntentName string
	Network    *Network

	Members []Member
}

// Network carries an exported intent's exact network layout, so a member's
// static address is still meaningful across hosts.
type Network struct {
	Subnet        string
	Gateway       string
	DockerIPRange string
}

// Member is one instance in the bundle.
type Member struct {
	Name string
	Kind string // "vm" | "container"

	// Role is this member's role within its intent; empty for a standalone bundle.
	Role string

	// StaticIP is this member's pinned address (VM only, CIDR form), empty if none.
	StaticIP string

	VM        *VM
	Container *Container
}

// VM is the VM half of a Member.
type VM struct {
	ImageRef  string
	Arch      string
	CPUs      int32
	MemoryMiB int64
	DiskGiB   int64

	// CloudInitContent is the resolved cloud-init user-data, whether the source used a raw config or a
	// named library entry; import always relaunches with it as ad hoc data so the bundle is self-contained.
	CloudInitContent string

	SSHPublicKeys []string
	Ports         []PortMapping

	// DiskFile is this member's qcow2 diff disk's path inside the bundle (relative to
	// the archive root), backed by its original base image rather than flattened.
	DiskFile string

	// BaseSHA256 is that base image's checksum on the exporting host, so the importing
	// host can tell its own copy is identical before rebasing onto it. Empty in older
	// bundles, and for a disk that has no base image.
	BaseSHA256 string

	// Mounts are the folders shared into the guest, archived alongside the disk.
	Mounts []Mount
}

// Mount is one folder shared into a VM's guest, with its contents.
type Mount struct {
	GuestPath string
	Tag       string // kept as it was, the guest's fstab travelled inside the disk
	ReadOnly  bool

	// Archive is this folder's tree's path prefix inside the bundle, empty if its
	// host directory couldn't be read at export time.
	Archive string
}

// Container is the container half of a Member.
type Container struct {
	ImageRef   string
	Env        map[string]string
	Entrypoint []string
	Cmd        []string
	Volumes    []VolumeMount
	Ports      []PortMapping
	Engine     string
}

// VolumeMount is one bind-mounted host directory, archived alongside the spec.
type VolumeMount struct {
	ContainerPath string
	ReadOnly      bool

	// Archive is this volume's directory tree's path prefix inside the
	// bundle, empty if its host directory didn't exist at export time.
	Archive string
}

type PortMapping struct {
	HostPort  int
	GuestPort int
	Protocol  string
}
