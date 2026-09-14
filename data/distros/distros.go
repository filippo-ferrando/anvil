// Package distros embeds anvil's built-in default VM image catalog.
package distros

import _ "embed"

//go:embed distribution-info.json
var Raw []byte
