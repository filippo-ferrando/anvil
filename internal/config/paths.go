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
