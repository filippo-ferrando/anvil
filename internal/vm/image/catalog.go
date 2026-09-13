// Package image resolves VM base images: the built-in multi-distro catalog,
// runtime-added mirrors (see WithMirrors and the plan's "Image mirrors"
// section), and the on-disk prepared/instance vault.
package image

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/anvil-project/anvil/data/distros"
)

// CurrentSchemaVersion is the manifest schema this build understands.
// Loader rejects a manifest whose SchemaVersion has a different major
// version than this (mirror manifests share the same schema — see the
// plan's "Image mirrors" section).
const CurrentSchemaVersion = 1

// DistroEntry describes one fetchable base image.
type DistroEntry struct {
	ID          string `json:"id"`   // e.g. "ubuntu-24.04"
	Name        string `json:"name"` // e.g. "Ubuntu 24.04 LTS"
	Distro      string `json:"distro"`
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	MinDiskGiB  int64  `json:"min_disk_gib"`
	DefaultUser string `json:"default_user"` // the account cloud-init sets up by default, for `anvil shell`/`exec`; best-effort, see distribution-info.json's notes
}

// Manifest is the on-disk shape of both the embedded default catalog and
// any runtime-added mirror manifest (anvil mirror add --kind vm
// --manifest-url ...) — the same schema serves both.
type Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	Distros       []DistroEntry `json:"distros"`
}

type catalogEntry struct {
	DistroEntry
	Priority int
}

// Catalog resolves a distro/version/arch selector to a fetchable
// DistroEntry, merging the built-in manifest (priority 0) with any
// runtime-added mirrors (see WithMirrors). On a collision (same id+arch)
// the highest-priority entry wins.
type Catalog struct {
	entries []catalogEntry
}

// LoadEmbedded parses the build's embedded default distro manifest.
func LoadEmbedded() (*Catalog, error) {
	return loadManifest(distros.Raw, 0)
}

func loadManifest(raw []byte, priority int) (*Catalog, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("image: parsing manifest: %w", err)
	}
	if m.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("image: manifest schema_version %d unsupported (expected %d)", m.SchemaVersion, CurrentSchemaVersion)
	}
	entries := make([]catalogEntry, len(m.Distros))
	for i, e := range m.Distros {
		entries[i] = catalogEntry{DistroEntry: e, Priority: priority}
	}
	return &Catalog{entries: entries}, nil
}

// FetchManifest downloads and validates a mirror manifest (same schema as
// the embedded catalog) from url, returning the raw bytes to cache
// alongside the mirror record (see store.Mirror.ManifestJSON) — validating
// here, at `anvil mirror add` time, means a bad manifest is rejected
// immediately instead of surfacing as a confusing failure at launch time.
func FetchManifest(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("image: fetching manifest %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image: fetching manifest %s: unexpected status %s", url, resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("image: reading manifest %s: %w", url, err)
	}
	if _, err := loadManifest(raw, 0); err != nil {
		return nil, fmt.Errorf("image: manifest at %s is invalid: %w", url, err)
	}
	return raw, nil
}

// MirrorManifest is one runtime-added mirror's cached manifest content,
// ready to merge into a Catalog. The daemon is responsible for populating
// ManifestJSON (fetched once when the mirror is added, cached in the
// mirrors bbolt bucket — see internal/store/mirrors.go) so building the
// effective catalog never needs a network call.
type MirrorManifest struct {
	ManifestJSON string
	Priority     int
}

// WithMirrors returns a new Catalog with mirrors' entries merged on top of
// c's. c itself is left unmodified.
func (c *Catalog) WithMirrors(mirrors []MirrorManifest) (*Catalog, error) {
	merged := &Catalog{entries: append([]catalogEntry(nil), c.entries...)}
	for _, m := range mirrors {
		cat, err := loadManifest([]byte(m.ManifestJSON), m.Priority)
		if err != nil {
			return nil, fmt.Errorf("image: merging mirror manifest: %w", err)
		}
		merged.entries = append(merged.entries, cat.entries...)
	}
	return merged, nil
}

// List returns the effective catalog: one entry per (id, arch), the
// highest-priority one on a collision.
func (c *Catalog) List() []DistroEntry {
	winners := c.winners()
	out := make([]DistroEntry, 0, len(winners))
	for _, e := range winners {
		out = append(out, e.DistroEntry)
	}
	return out
}

// Find resolves an alias like "ubuntu-24.04" to its highest-priority entry
// for the given arch (defaulting to "x86_64").
func (c *Catalog) Find(id, arch string) (DistroEntry, error) {
	if arch == "" {
		arch = "x86_64"
	}
	var best *catalogEntry
	for i := range c.entries {
		e := &c.entries[i]
		if e.ID != id || e.Arch != arch {
			continue
		}
		if best == nil || e.Priority > best.Priority {
			best = e
		}
	}
	if best == nil {
		return DistroEntry{}, fmt.Errorf("image: no catalog entry for %q (arch %s)", id, arch)
	}
	return best.DistroEntry, nil
}

// winners collapses c.entries to one per (id, arch) key, the
// highest-priority entry winning ties broken by manifest order.
func (c *Catalog) winners() []catalogEntry {
	type key struct{ id, arch string }
	best := make(map[key]catalogEntry)
	order := make([]key, 0)
	for _, e := range c.entries {
		k := key{e.ID, e.Arch}
		if existing, ok := best[k]; !ok || e.Priority > existing.Priority {
			if !ok {
				order = append(order, k)
			}
			best[k] = e
		}
	}
	out := make([]catalogEntry, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}
