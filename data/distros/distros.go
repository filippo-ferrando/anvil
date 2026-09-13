// Package distros embeds anvil's built-in default VM image catalog
// (distribution-info.json) into the binary, so a fresh install has a
// working set of cloud-init images without any network fetch beyond the
// image downloads themselves. See internal/vm/image for the loader that
// parses this and internal/vm/image's Manifest type for the schema.
package distros

import _ "embed"

//go:embed distribution-info.json
var Raw []byte
