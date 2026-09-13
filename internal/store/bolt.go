// Package store is anvil's bbolt-backed instance registry. Transactional
// writes avoid the partial-write corruption risk plain per-instance JSON
// files would carry, which matters because the reconciliation pass
// (internal/vm's backend, on daemon startup) trusts this persisted state.
//
// High-churn ephemeral runtime state (pid, qmp socket path) deliberately
// does NOT live here — see internal/config.InstanceDir — to avoid lock
// contention between the registry and frequent runtime-state updates.
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/anvil-project/anvil/internal/instance"
)

var (
	bucketInstances     = []byte("instances")      // id -> json(record)
	bucketInstanceNames = []byte("instance_names") // name -> id
)

// record is the on-disk shape of a Spec — kept separate from
// instance.Spec so storage format can evolve without touching the domain
// type callers work with directly.
type record struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Kind      instance.Kind           `json:"kind"`
	State     instance.State          `json:"state"`
	CreatedAt time.Time               `json:"created_at"`
	Labels    map[string]string       `json:"labels"`
	VM        *instance.VMSpec        `json:"vm,omitempty"`
	Container *instance.ContainerSpec `json:"container,omitempty"`
}

func toRecord(s *instance.Spec) record {
	return record{
		ID: s.ID, Name: s.Name, Kind: s.Kind, State: s.State,
		CreatedAt: s.CreatedAt, Labels: s.Labels, VM: s.VM, Container: s.Container,
	}
}

func (r record) toSpec() *instance.Spec {
	return &instance.Spec{
		ID: r.ID, Name: r.Name, Kind: r.Kind, State: r.State,
		CreatedAt: r.CreatedAt, Labels: r.Labels, VM: r.VM, Container: r.Container,
	}
}

type Store struct {
	db *bbolt.DB
}

// Open opens (creating if needed) the bbolt database at path and ensures
// the buckets this package uses exist.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{bucketInstances, bucketInstanceNames, bucketCloudInit, bucketMirrors, bucketIntents, bucketIntentNames, bucketHosts} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: initializing buckets: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// PutInstance inserts or replaces spec's record, keeping the name->id index
// in sync (including removing a stale index entry if spec was renamed,
// though anvil doesn't expose a rename operation as of M1).
func (s *Store) PutInstance(spec *instance.Spec) error {
	data, err := json.Marshal(toRecord(spec))
	if err != nil {
		return fmt.Errorf("store: encoding instance %s: %w", spec.ID, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bucketInstances).Put([]byte(spec.ID), data); err != nil {
			return err
		}
		return tx.Bucket(bucketInstanceNames).Put([]byte(spec.Name), []byte(spec.ID))
	})
}

// GetByID returns a copy of the instance with the given ID.
func (s *Store) GetByID(id string) (*instance.Spec, error) {
	var rec record
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucketInstances).Get([]byte(id))
		if data == nil {
			return fmt.Errorf("store: no instance with id %q", id)
		}
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	return rec.toSpec(), nil
}

// GetByName returns a copy of the instance with the given name.
func (s *Store) GetByName(name string) (*instance.Spec, error) {
	var id string
	err := s.db.View(func(tx *bbolt.Tx) error {
		idBytes := tx.Bucket(bucketInstanceNames).Get([]byte(name))
		if idBytes == nil {
			return fmt.Errorf("store: no instance named %q", name)
		}
		id = string(idBytes)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetByID(id)
}

// List returns every instance, optionally filtered by kind (pass "" for
// no filter).
func (s *Store) List(kindFilter instance.Kind) ([]*instance.Spec, error) {
	var out []*instance.Spec
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketInstances).ForEach(func(_, data []byte) error {
			var rec record
			if err := json.Unmarshal(data, &rec); err != nil {
				return err
			}
			if kindFilter == "" || rec.Kind == kindFilter {
				out = append(out, rec.toSpec())
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteByID removes an instance's record and its name index entry.
func (s *Store) DeleteByID(id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketInstances)
		data := bucket.Get([]byte(id))
		if data == nil {
			return fmt.Errorf("store: no instance with id %q", id)
		}
		var rec record
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		return tx.Bucket(bucketInstanceNames).Delete([]byte(rec.Name))
	})
}
