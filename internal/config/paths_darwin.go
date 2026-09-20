//go:build darwin

package config

// /usr/local is used instead of a Homebrew-prefix guess (e.g. /opt/homebrew)
// because packaging/macos installs a plain tarball, not a Homebrew formula. Provisional: revisit if that changes.
const (
	RunDir   = "/usr/local/var/run/anvil"
	StateDir = "/usr/local/var/anvil"
	CacheDir = "/usr/local/var/cache/anvil"
	ConfDir  = "/usr/local/etc/anvil"
)
