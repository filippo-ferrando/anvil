//go:build linux

package vm

import (
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

type mirrorList struct {
	noMirrors
	mirrors []store.Mirror
}

func (m *mirrorList) ListMirrors(store.MirrorKind) ([]store.Mirror, error) { return m.mirrors, nil }

// EffectiveCatalog reuses the merged catalog until the mirror set changes.
func TestEffectiveCatalogCaches(t *testing.T) {
	cat, err := image.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version": 1, "distros": [{"id": "ubuntu-24.04", "arch": "x86_64", "url": "https://a.example/u.img", "min_disk_gib": 3}]}`
	src := &mirrorList{mirrors: []store.Mirror{{Enabled: true, ManifestJSON: manifest, Priority: 10}}}
	b := &Backend{Catalog: cat, Source: src}

	first, err := b.EffectiveCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := b.EffectiveCatalog(); again != first {
		t.Error("expected the cached catalog for an unchanged mirror set")
	}
	src.mirrors[0].Priority = 20
	changed, _ := b.EffectiveCatalog()
	if changed == first {
		t.Error("expected a new merge after a mirror changed")
	}
	if e, err := changed.Find("ubuntu-24.04", "x86_64"); err != nil || e.URL != "https://a.example/u.img" {
		t.Errorf("Find = %+v, %v", e, err)
	}
}
