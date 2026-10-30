//go:build linux

package config

import "os"

const (
	RunDir   = "/run/anvil"
	StateDir = "/var/lib/anvil"
	CacheDir = "/var/cache/anvil"

	guiToolsDir = "/usr/lib/anvil/gui"
)

// FirecrackerDir holds the static firecracker and jailer the anvild package ships.
// ANVIL_FIRECRACKER_DIR overrides it, for development builds.
func FirecrackerDir() string {
	if dir := os.Getenv("ANVIL_FIRECRACKER_DIR"); dir != "" {
		return dir
	}
	return "/usr/lib/anvil/firecracker"
}

// GuestToolsDir holds anvil-guest, the static binary that runs commands inside ephemeral
// clones. ANVIL_GUEST_TOOLS overrides it, for development builds.
func GuestToolsDir() string {
	if dir := os.Getenv("ANVIL_GUEST_TOOLS"); dir != "" {
		return dir
	}
	return "/usr/lib/anvil/guest"
}
