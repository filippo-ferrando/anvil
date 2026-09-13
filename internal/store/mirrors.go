package store

import (
	"encoding/json"
	"fmt"
	"sort"

	"go.etcd.io/bbolt"
)

var bucketMirrors = []byte("mirrors") // name -> json(Mirror)

// MirrorKind distinguishes a VM image mirror (a distribution-info.json
// shaped manifest URL) from a container registry mirror (applied via a
// registries.conf.d drop-in once the Podman backend lands in M3). It's its
// own type, not instance.Kind, since a mirror isn't an instance.
type MirrorKind string

const (
	MirrorKindVM        MirrorKind = "vm"
	MirrorKindContainer MirrorKind = "container"
)

// Mirror is a runtime-added image source, on top of the embedded default
// catalog (data/distros/distribution-info.json). See the plan's "Image
// mirrors" section: a VM mirror's ManifestURL points at a manifest in the
// same schema as the embedded catalog; a container mirror's Registry/
// MirrorOf/Insecure fields describe a registries.conf.d entry.
type Mirror struct {
	Name         string
	Kind         MirrorKind
	ManifestURL  string // VM mirrors: where the manifest was fetched from
	ManifestJSON string // VM mirrors: the manifest content, cached at add time so resolving a catalog never needs a network call
	Registry     string // container mirrors: host[:port]
	MirrorOf     string // container mirrors: upstream this mirrors, optional
	Insecure     bool   // container mirrors
	Priority     int    // higher wins on a catalog entry collision
	Enabled      bool
}

// PutMirror inserts or replaces a mirror record by name.
func (s *Store) PutMirror(m Mirror) error {
	if m.Name == "" {
		return fmt.Errorf("store: mirror name must not be empty")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encoding mirror %s: %w", m.Name, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMirrors).Put([]byte(m.Name), data)
	})
}

func (s *Store) GetMirror(name string) (Mirror, error) {
	var m Mirror
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucketMirrors).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("store: no mirror named %q", name)
		}
		return json.Unmarshal(data, &m)
	})
	return m, err
}

// ListMirrors returns every mirror, optionally filtered by kind (pass ""
// for no filter), sorted by descending priority then name so callers that
// want "highest priority first" don't have to re-sort.
func (s *Store) ListMirrors(kindFilter MirrorKind) ([]Mirror, error) {
	var out []Mirror
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMirrors).ForEach(func(_, data []byte) error {
			var m Mirror
			if err := json.Unmarshal(data, &m); err != nil {
				return err
			}
			if kindFilter == "" || m.Kind == kindFilter {
				out = append(out, m)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Store) DeleteMirror(name string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMirrors)
		if bucket.Get([]byte(name)) == nil {
			return fmt.Errorf("store: no mirror named %q", name)
		}
		return bucket.Delete([]byte(name))
	})
}

func (s *Store) SetMirrorEnabled(name string, enabled bool) error {
	m, err := s.GetMirror(name)
	if err != nil {
		return err
	}
	m.Enabled = enabled
	return s.PutMirror(m)
}
