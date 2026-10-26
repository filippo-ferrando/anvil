// Package config centralizes the fixed on-disk layout anvild uses.
// RunDir/StateDir/CacheDir/ConfDir are OS-specific; everything derived from them here is shared.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// GUIRelayDir is the host side of a GUI-enabled container's /run/anvil-gui.
func GUIRelayDir(id string) string { return filepath.Join(RunDir, "gui", id) }

// GUIToolsDir holds the static waypipe and xwayland-satellite shared into VMs.
// ANVIL_GUI_TOOLS overrides it, for development builds.
func GUIToolsDir() string {
	if dir := os.Getenv("ANVIL_GUI_TOOLS"); dir != "" {
		return dir
	}
	return guiToolsDir
}

// SocketPath is the daemon's gRPC listen address.
func SocketPath() string { return filepath.Join(RunDir, "anvild.sock") }

// DevHelperSocket is where anvil-devhelper.socket listens, for USB/PCI passthrough claims.
func DevHelperSocket() string { return filepath.Join(RunDir, "devhelper.sock") }

// DBPath is the bbolt registry file (instances, intents, mirrors, hosts, cloud-init, events).
func DBPath() string { return filepath.Join(StateDir, "anvil.db") }

// InstanceDir is where a single instance's mutable, non-registry state lives.
func InstanceDir(id string) string { return filepath.Join(StateDir, "instances", id) }

// JailerDir holds the jailer's per-VM chroots, on the same filesystem as the instance disks
// so they can be hard-linked in.
func JailerDir() string { return filepath.Join(StateDir, "jailer") }

// JailerExe is the jailer copy with file capabilities that anvild.service prepares, runnable
// only by the anvil user. Firecracker then starts without anvild's capabilities.
func JailerExe() string { return filepath.Join(StateDir+"-jailer", "jailer") }

// PreparedImageDir holds downloaded and checksum-verified base images.
func PreparedImageDir() string { return filepath.Join(CacheDir, "images", "prepared") }

// FirecrackerImageDir holds Firecracker's guest kernels and raw copies of the prepared images.
func FirecrackerImageDir() string { return filepath.Join(CacheDir, "images", "firecracker") }

// MigrateKnownHostsPath is the shared SSH known_hosts file for migration targets.
func MigrateKnownHostsPath() string { return filepath.Join(StateDir, "migrate-known-hosts") }

// MigrateStagingDir holds a VM disk mid-flight while it's flattened for migration.
func MigrateStagingDir() string { return filepath.Join(CacheDir, "migrate-staging") }

// MigrateIdentityPath is the ed25519 keypair anvild uses for outbound migration SSH connections.
func MigrateIdentityPath() string { return filepath.Join(StateDir, ".ssh", "id_ed25519") }

// ExportStagingDir holds a bundle's extracted contents mid-import, before
// its disk/volumes are adopted into their permanent locations.
func ExportStagingDir() string { return filepath.Join(CacheDir, "export-staging") }

// ImportedVolumeDir is where an imported container's Nth bind-mounted
// volume's restored contents live permanently.
func ImportedVolumeDir(instanceName string, idx int) string {
	return filepath.Join(StateDir, "container-volumes", instanceName, fmt.Sprint(idx))
}

// MountedFolderDir is the same, for a VM's Nth shared folder.
func MountedFolderDir(instanceName string, idx int) string {
	return filepath.Join(StateDir, "vm-mounts", instanceName, fmt.Sprint(idx))
}

// FreeDir returns path, or path with the first "-N" suffix that doesn't exist yet, so data
// restored under a reused instance name never lands on an older copy.
func FreeDir(path string) string {
	candidate := path
	for n := 2; ; n++ {
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", path, n)
	}
}
