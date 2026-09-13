package store

import (
	"encoding/json"
	"fmt"
	"sort"

	"go.etcd.io/bbolt"
)

var bucketHosts = []byte("hosts") // alias -> json(Host)

// Host is a saved SSH destination `anvil migrate --to <alias>` can
// resolve against — see the plan's Migration section. Adding one grants
// no trust by itself, it's just a shortcut for whatever real SSH access
// already exists to Target.
type Host struct {
	Alias    string
	Target   string // "user@host[:port]"
	Identity string // optional path to a private key; empty uses ssh's own default identity resolution
}

func (s *Store) PutHost(h Host) error {
	if h.Alias == "" || h.Target == "" {
		return fmt.Errorf("store: host must have an alias and a target")
	}
	data, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("store: encoding host %s: %w", h.Alias, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketHosts).Put([]byte(h.Alias), data)
	})
}

func (s *Store) GetHost(alias string) (Host, error) {
	var h Host
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucketHosts).Get([]byte(alias))
		if data == nil {
			return fmt.Errorf("store: no host named %q", alias)
		}
		return json.Unmarshal(data, &h)
	})
	return h, err
}

// ListHosts returns every saved host, sorted by alias.
func (s *Store) ListHosts() ([]Host, error) {
	var out []Host
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketHosts).ForEach(func(_, data []byte) error {
			var h Host
			if err := json.Unmarshal(data, &h); err != nil {
				return err
			}
			out = append(out, h)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

func (s *Store) DeleteHost(alias string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketHosts)
		if bucket.Get([]byte(alias)) == nil {
			return fmt.Errorf("store: no host named %q", alias)
		}
		return bucket.Delete([]byte(alias))
	})
}
