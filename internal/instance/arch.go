package instance

import (
	"runtime"
	"strings"
)

// NormalizeArch maps a Go or `uname -m` arch name to VMSpec.Arch's naming
// ("x86_64", "aarch64").
func NormalizeArch(s string) string {
	switch s := strings.TrimSpace(s); s {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return s
	}
}

// HostArch is this host's CPU arch in VMSpec.Arch's naming.
func HostArch() string { return NormalizeArch(runtime.GOARCH) }
