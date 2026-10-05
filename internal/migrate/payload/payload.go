// Package payload defines the JSON data anvil migrate ships to the target
// host's local anvil CLI over SSH.
package payload

// Payload is everything needed to relaunch one migrated instance on the
// target host.
type Payload struct {
	Name      string
	Kind      string // "vm" | "container"
	VM        *VM
	Container *Container

	// IntentName/Role are empty for a standalone migration; when set,
	// migrate-import joins (or creates) that intent on the target.
	IntentName string
	Role       string

	// IntentNetwork/StaticIP pin the target to the source's exact
	// network/address. StaticIP (CIDR form) is VM-only.
	IntentNetwork *IntentNetwork
	StaticIP      string
}

// IntentNetwork carries a migrated intent's exact network layout.
type IntentNetwork struct {
	Subnet        string
	Gateway       string
	DockerIPRange string
}

// VM mirrors the subset of instance.VMSpec a migrated relaunch needs.
// RemoteDiskPath is where the disk was uploaded to on the target. BaseSHA256, when
// set, marks that disk as a delta on top of the base image with this checksum.
type VM struct {
	ImageRef       string // display/record purposes only on the target
	Arch           string
	CPUs           int32
	MemoryMiB      int64
	DefaultUser    string
	RemoteDiskPath string
	BaseSHA256     string

	// RemoteBaseImagePath is where the base image itself was uploaded, set only
	// when the target didn't already have it cached.
	RemoteBaseImagePath string

	// Mounts are the guest's shared folders, recreated on the target.
	Mounts []Mount
}

// Mount is one folder shared into a VM's guest, with its contents.
type Mount struct {
	GuestPath string
	Tag       string // kept as it was, the guest's fstab travelled inside the disk
	ReadOnly  bool

	// RemoteDataPath is where this folder's contents were uploaded on the target.
	// A folder whose contents can't be read on the source is left out entirely,
	// since a mount with no directory behind it stops the VM from starting.
	RemoteDataPath string
}

// Container mirrors the subset of instance.ContainerSpec a migrated
// relaunch needs; the target re-pulls ImageRef itself, no transfer.
type Container struct {
	ImageRef    string
	Env         map[string]string
	Entrypoint  []string
	Cmd         []string
	Volumes     []VolumeMount
	Ports       []PortMapping
	NetworkMode string
	Engine      string
}

type VolumeMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool

	// RemoteDataPath is where this volume's contents were uploaded on the target.
	// When it is empty the target bind-mounts HostPath as it stands.
	RemoteDataPath string
}

type PortMapping struct {
	HostPort  int
	GuestPort int
	Protocol  string
}
