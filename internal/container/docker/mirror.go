package docker

import "strings"

// RegistryMirror is one enabled container registry mirror rule: a pull that
// would otherwise go to MirrorOf (or Docker Hub, if MirrorOf is empty) gets
// redirected to Registry instead. Kept as its own small type here, not
// store.Mirror directly, for the same reason CreateContainerParams stays
// decoupled from instance.ContainerSpec: this package doesn't import
// internal/store, so it stays testable without network access — the
// translation from store.Mirror happens in internal/container
// (docker_backend.go), which already depends on both.
type RegistryMirror struct {
	Registry string
	MirrorOf string
}

// defaultRegistry is where any image reference with no explicit registry
// host resolves, same as Docker itself: "nginx:alpine" and
// "bitnami/postgres" are both implicitly Docker Hub.
const defaultRegistry = "docker.io"

// splitRegistryHost splits ref into an explicit registry host (if any) and
// the rest of the reference, using the same convention Docker's own
// reference parser does: the first "/"-delimited path segment counts as a
// registry host only if it contains a "." or ":" or is exactly
// "localhost" — otherwise the whole ref is read as living on the implicit
// default registry, so "library/nginx" is NOT mistaken for registry host
// "library".
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

// ResolveMirror rewrites ref to pull through the highest-priority enabled
// mirror configured for its upstream registry, or returns ref unchanged if
// none applies. mirrors is expected pre-filtered to enabled entries and
// pre-sorted by descending priority (see store.ListMirrors, which already
// sorts that way) — the first match here wins.
//
// This is deliberately a client-side rewrite, not a rewrite of dockerd's
// own daemon.json — Docker's daemon-level "registry-mirrors" setting only
// ever mirrors Docker Hub specifically, it has no concept of mirroring an
// arbitrary registry like quay.io the way this needs to support, and
// editing a system-wide daemon config file (plus reloading dockerd) for a
// per-anvil-instance concern is a much bigger blast radius than just
// resolving the reference before pulling. Podman's registries.conf.d
// mechanism (once that backend lands) can do the daemon-level version
// properly; this is Docker's alternative for the same feature.
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
