package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

var bucketWarm = []byte("warm") // key -> json(WarmSnapshot)

// WarmSnapshot records one warm snapshot, whose files live in config.WarmDir(Key). It
// belongs to no instance and is never exported or migrated.
type WarmSnapshot struct {
	Key                string
	Distro             string
	Arch               string
	CPUs               int
	MemoryMiB          int64
	DiskGiB            int64
	FirecrackerVersion string
	SizeBytes          int64 // disk space its files take
	CreatedAt          time.Time
	LastUsedAt         time.Time
}

func (s *Store) PutWarm(w WarmSnapshot) error {
	if w.Key == "" {
		return fmt.Errorf("store: warm snapshot key must not be empty")
	}
	data, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("store: encoding warm snapshot %s: %w", w.Key, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketWarm).Put([]byte(w.Key), data)
	})
}

func (s *Store) GetWarm(key string) (WarmSnapshot, error) {
	var w WarmSnapshot
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucketWarm).Get([]byte(key))
		if data == nil {
			return fmt.Errorf("store: no warm snapshot %q: %w", key, instance.ErrNotFound)
		}
		return json.Unmarshal(data, &w)
	})
	return w, err
}

// ListWarm returns every warm snapshot, sorted by distro, then size, then key.
func (s *Store) ListWarm() ([]WarmSnapshot, error) {
	var out []WarmSnapshot
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketWarm).ForEach(func(_, data []byte) error {
			var w WarmSnapshot
			if err := json.Unmarshal(data, &w); err != nil {
				return err
			}
			out = append(out, w)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Distro != b.Distro {
			return a.Distro < b.Distro
		}
		if a.CPUs != b.CPUs || a.MemoryMiB != b.MemoryMiB {
			return a.CPUs < b.CPUs || (a.CPUs == b.CPUs && a.MemoryMiB < b.MemoryMiB)
		}
		return a.Key < b.Key
	})
	return out, err
}

func (s *Store) DeleteWarm(key string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketWarm)
		if bucket.Get([]byte(key)) == nil {
			return fmt.Errorf("store: no warm snapshot %q: %w", key, instance.ErrNotFound)
		}
		return bucket.Delete([]byte(key))
	})
}
