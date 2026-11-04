// Package distros embeds anvil's default VM image catalog, fetched at build time from
// the anvil/catalog repo by scripts/fetch-catalog.sh (`make catalog`).
package distros

import (
	"embed"
	"io/fs"
)

// The build fails here when the catalog was never fetched: run `make catalog`.
//
//go:embed distros_*.json firecracker_*.json
var files embed.FS

// Manifests returns every embedded manifest: the per-arch distro lists first, then the
// Firecracker sections (guest kernel and bootable distro ids per arch).
func Manifests() ([][]byte, error) {
	var out [][]byte
	for _, pattern := range []string{"distros_*.json", "firecracker_*.json"} {
		names, err := fs.Glob(files, pattern)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			raw, err := files.ReadFile(n)
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		}
	}
	return out, nil
}
