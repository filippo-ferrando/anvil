//go:build darwin

package config

// /usr/local is used rather than a Homebrew-prefix guess (e.g.
// /opt/homebrew) since the macOS package (packaging/macos) installs a
// plain tarball, not a Homebrew formula, and /usr/local exists on every
// Mac regardless of whether Homebrew is present. Provisional: revisit if
// this ever needs to coexist with a real Homebrew install.
const (
	RunDir   = "/usr/local/var/run/anvil"
	StateDir = "/usr/local/var/anvil"
	CacheDir = "/usr/local/var/cache/anvil"
	ConfDir  = "/usr/local/etc/anvil"
)
