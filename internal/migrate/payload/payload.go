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
// RemoteDiskPath is where the flattened disk was scp'd to on the target.
type VM struct {
	ImageRef       string // display/record purposes only on the target
	Arch           string
	CPUs           int32
	MemoryMiB      int64
	DefaultUser    string
	RemoteDiskPath string
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
}

type PortMapping struct {
	HostPort  int
	GuestPort int
	Protocol  string
}
