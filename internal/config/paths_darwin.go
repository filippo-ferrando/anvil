//go:build darwin

package config

// /usr/local is used instead of a Homebrew-prefix guess (e.g. /opt/homebrew).
// Provisional: macOS packaging does not exist yet.
const (
	RunDir   = "/usr/local/var/run/anvil"
	StateDir = "/usr/local/var/anvil"
	CacheDir = "/usr/local/var/cache/anvil"

	guiToolsDir = "/usr/local/lib/anvil/gui"
)
