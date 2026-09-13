// Package payload is the plain, JSON-only data anvil migrate ships to the
// target host's own local anvil CLI (see internal/migrate and the
// `anvil migrate-import` command in internal/cli/commands). It
// deliberately depends on nothing else in this project (not
// internal/instance, not the generated proto types): the source daemon
// builds one of these and pipes it, as JSON, over SSH's stdin to
// `anvil migrate-import` on the target — never as command-line
// arguments, which would need careful shell-quoting for arbitrary spec
// content (image refs, env values, cloud-init text, ...) that a fixed,
// argument-free remote command sidesteps entirely.
package payload

// Payload is everything needed to relaunch one migrated instance on the
// target host.
type Payload struct {
	Name      string
	Kind      string // "vm" | "container"
	VM        *VM
	Container *Container

	// IntentName/Role are both empty for a standalone migration. When
	// set (migrating a whole intent, M7 — see
	// internal/migrate.Manager.migrateIntent), migrate-import passes
	// them straight through to the target's own Launch call, which
	// joins (or creates) that same-named intent there exactly like a
	// normal `anvil launch --intent` would.
	IntentName string
	Role       string

	// IntentNetwork/StaticIP are both nil/empty for a standalone
	// migration, or for an intent with no shared network at all. When
	// set, migrate-import passes them through as the target Launch
	// call's pinned_subnet/pinned_gateway/pinned_docker_ip_range/
	// pinned_static_ip, so the target's newly-created intent reuses the
	// source's exact network instead of auto-allocating a fresh one —
	// see internal/migrate.Manager.migrateIntent's doc comment for why:
	// a migrated VM's disk skips cloud-init entirely on relaunch, so its
	// already-baked-in static network config (and whatever peer
	// /etc/hosts entries it already has) can only keep working if
	// nothing about the network actually changes underneath it.
	// StaticIP (CIDR form, e.g. "10.55.201.4/24") is VM-only — a
	// container's networking is re-established fresh at every launch
	// anyway, nothing baked-in to preserve, so it's simply left to the
	// target's own engine to assign as usual.
	IntentNetwork *IntentNetwork
	StaticIP      string
}

// IntentNetwork carries a migrated intent's exact network layout — see
// Payload.IntentNetwork.
type IntentNetwork struct {
	Subnet        string
	Gateway       string
	DockerIPRange string
}

// VM mirrors the subset of instance.VMSpec a migrated relaunch needs.
// RemoteDiskPath is where internal/migrate already scp'd the flattened
// disk to on the target before sending this payload — anvil migrate-import
// passes it straight through as the target launch's --from-disk.
type VM struct {
	ImageRef       string // display/record purposes only on the target, not resolved against its catalog
	Arch           string
	CPUs           int32
	MemoryMiB      int64
	DefaultUser    string
	RemoteDiskPath string
}

// Container mirrors the subset of instance.ContainerSpec a migrated
// relaunch needs. No disk/image transfer happens for a container: the
// target re-pulls ImageRef itself, which only actually works for a
// registry-hosted image, not one that only ever existed as a local build
// on the source — an accepted v1 limitation, see PLAN.md.
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
