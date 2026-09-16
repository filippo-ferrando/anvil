// Package config centralizes the fixed on-disk layout anvild uses.
// RunDir/StateDir/CacheDir/ConfDir are OS-specific (see paths_linux.go,
// paths_darwin.go); everything derived from them here is shared.
package config

import "path/filepath"

// SocketPath is the daemon's gRPC listen address.
func SocketPath() string { return filepath.Join(RunDir, "anvild.sock") }

// DBPath is the bbolt registry file (instances/intents/images/mirrors buckets).
func DBPath() string { return filepath.Join(StateDir, "anvil.db") }

// InstanceDir is where a single instance's mutable, non-registry state lives.
func InstanceDir(id string) string { return filepath.Join(StateDir, "instances", id) }

// PreparedImageDir holds downloaded and checksum-verified base images.
func PreparedImageDir() string { return filepath.Join(CacheDir, "images", "prepared") }

// CloudInitDir holds the saved cloud-init user-data library.
func CloudInitDir() string { return filepath.Join(StateDir, "cloud-init") }

// MigrateKnownHostsPath is the shared SSH known_hosts file for migration targets.
func MigrateKnownHostsPath() string { return filepath.Join(StateDir, "migrate-known-hosts") }

// MigrateStagingDir holds a VM disk mid-flight while it's flattened for migration.
func MigrateStagingDir() string { return filepath.Join(CacheDir, "migrate-staging") }

// MigrateIdentityPath is the ed25519 keypair anvild uses for outbound migration SSH connections.
func MigrateIdentityPath() string { return filepath.Join(StateDir, ".ssh", "id_ed25519") }
