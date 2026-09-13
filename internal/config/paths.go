// Package config centralizes the on-disk layout anvild uses. All paths are
// fixed (no per-user XDG variance) because anvild always runs as the
// dedicated "anvil" system user, never per-invoking-user.
package config

import "path/filepath"

const (
	RunDir   = "/run/anvil"
	StateDir = "/var/lib/anvil"
	CacheDir = "/var/cache/anvil"
	ConfDir  = "/etc/anvil"
)

// SocketPath is the daemon's gRPC listen address.
func SocketPath() string { return filepath.Join(RunDir, "anvild.sock") }

// DBPath is the bbolt registry file (instances/intents/images/mirrors buckets).
func DBPath() string { return filepath.Join(StateDir, "anvil.db") }

// InstanceDir is where a single instance's mutable, non-registry state lives
// (runtime.json, disk.qcow2, seed.iso, logs) — kept outside bbolt so
// high-churn writes don't contend with the registry's transaction lock.
func InstanceDir(id string) string { return filepath.Join(StateDir, "instances", id) }

// PreparedImageDir holds downloaded + checksum-verified base images, shared
// (via qcow2 backing files) across every instance launched from them.
func PreparedImageDir() string { return filepath.Join(CacheDir, "images", "prepared") }

// CloudInitDir holds the saved cloud-init user-data library (the CLI's
// `anvil cloud-init *` / the TUI's Cloud Init view both operate on this).
func CloudInitDir() string { return filepath.Join(StateDir, "cloud-init") }

// MigrateKnownHostsPath is one shared SSH known_hosts file for every
// `anvil migrate` target — see internal/migrate. Unlike a VM's own
// per-instance known_hosts (an ephemeral SLIRP-forwarded port that gets
// reused across unrelated instances), a migration target is a
// deliberately-added, persistent host, so there's no equivalent
// port-reuse confusion to design around.
func MigrateKnownHostsPath() string { return filepath.Join(StateDir, "migrate-known-hosts") }

// MigrateStagingDir holds a VM disk mid-flight while it's being flattened
// for `anvil migrate`, before it's shipped to the target and removed —
// see internal/migrate.Manager.Migrate.
func MigrateStagingDir() string { return filepath.Join(CacheDir, "migrate-staging") }

// MigrateIdentityPath is the passwordless ed25519 keypair anvild uses for
// its own outbound `anvil migrate` SSH connections when a host (see
// store.Host) doesn't specify its own --identity — generated once at
// package-install time (packaging/anvild.install), not by anvild itself.
// This is deliberately the "anvil" system user's own conventional
// ~/.ssh/id_ed25519 path (StateDir doubles as that user's home
// directory, see packaging/anvil.sysusers), the same file ssh's own
// default identity resolution would already find on its own — passing it
// explicitly instead of relying on that default resolution is the same
// fix already applied once for a near-identical bug on the CLI side (see
// internal/cli/commands/ssh.go's own doc comments): depending on $HOME
// being set correctly for a non-interactive/service process is exactly
// the kind of thing that quietly breaks.
func MigrateIdentityPath() string { return filepath.Join(StateDir, ".ssh", "id_ed25519") }
