// Package version reports the build version Go stamps into the binary from git.
package version

import "runtime/debug"

// Get returns the module version (a tag like "v1.1.1", a pseudo-version for an
// untagged build, "+dirty" for a modified tree) and the short commit.
func Get() (version, commit string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			commit = s.Value[:min(12, len(s.Value))]
		}
	}
	return info.Main.Version, commit
}
