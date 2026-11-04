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

// File is one embedded manifest and its file name in the catalog repo.
type File struct {
	Name string
	Raw  []byte
}

// Files returns every embedded manifest: the per-arch distro lists first, then the
// Firecracker sections (guest kernel and bootable distro ids per arch).
func Files() ([]File, error) {
	var out []File
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
			out = append(out, File{Name: n, Raw: raw})
		}
	}
	return out, nil
}

// Manifests returns the content of every manifest Files returns, in the same order.
func Manifests() ([][]byte, error) {
	all, err := Files()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(all))
	for i, f := range all {
		out[i] = f.Raw
	}
	return out, nil
}
