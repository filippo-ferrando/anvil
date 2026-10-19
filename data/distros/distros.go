// Package distros embeds anvil's built-in default VM image catalog.
package distros

import _ "embed"

//go:embed distribution-info.json
var Raw []byte

// FirecrackerRaw is a manifest with only a firecracker section: the guest kernel per
// arch, and which distro ids of Raw boot under Firecracker.
//
//go:embed firecracker.json
var FirecrackerRaw []byte
