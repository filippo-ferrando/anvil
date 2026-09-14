package docker

import "strings"

// RegistryMirror redirects a pull that would go to MirrorOf (or Docker Hub,
// if empty) to Registry instead.
type RegistryMirror struct {
	Registry string
	MirrorOf string
}

// defaultRegistry is where an image reference with no explicit registry
// host resolves, same as Docker itself.
const defaultRegistry = "docker.io"

// splitRegistryHost splits ref into an explicit registry host (if any) and
// the rest of the reference.
func splitRegistryHost(ref string) (registry, rest string) {
	slash := strings.Index(ref, "/")
	if slash == -1 {
		return "", ref
	}
	first := ref[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first, ref[slash+1:]
	}
	return "", ref
}

// ResolveMirror rewrites ref through the first matching mirror for its
// upstream registry, or returns ref unchanged if none applies.
func ResolveMirror(ref string, mirrors []RegistryMirror) string {
	registry, rest := splitRegistryHost(ref)
	upstream := registry
	if upstream == "" {
		upstream = defaultRegistry
	}
	for _, m := range mirrors {
		mirrorOf := m.MirrorOf
		if mirrorOf == "" {
			mirrorOf = defaultRegistry
		}
		if mirrorOf == upstream {
			return m.Registry + "/" + rest
		}
	}
	return ref
}
