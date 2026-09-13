package store

import (
	"encoding/json"
	"fmt"
	"sort"

	"go.etcd.io/bbolt"

	"github.com/anvil-project/anvil/internal/instance"
)

var (
	bucketIntents     = []byte("intents")      // id -> json(Intent)
	bucketIntentNames = []byte("intent_names") // name -> id
)

// IntentMember is one instance's membership in an Intent, referenced by
// ID rather than an embedded spec — the instances bucket stays the single
// source of truth for the spec itself, matching the plan's Intents
// section.
type IntentMember struct {
	InstanceID string
	Role       string
	Kind       instance.Kind
}

// IntentNetwork is the shared bridge network created for one intent, on
// top of a Docker (or, once it exists, Podman) network object — see
// PLAN.md's Intents section. Populated once, the first time any member is
// launched, and reused for every member after that.
type IntentNetwork struct {
	EngineNetworkName string // Docker/Podman network name (== "anvil-"+Intent.ID)
	BridgeInterface   string // the underlying Linux bridge's interface name, explicitly requested at create time
	Subnet            string // CIDR, e.g. "10.55.201.0/24"
	Gateway           string
	DockerIPRange     string // CIDR sub-range reserved for the engine's own IPAM, see internal/intent/ipam
}

// Intent is a named group of VM/container instances. Per the plan, every
// member of an intent shares one network (Network, nil until the first
// member is launched) and intents are isolated from each other by
// default.
type Intent struct {
	ID      string
	Name    string
	Members []IntentMember
	Network *IntentNetwork
}

// PutIntent inserts or replaces it by ID, keeping the name->id index in
// sync.
func (s *Store) PutIntent(it Intent) error {
	if it.ID == "" || it.Name == "" {
		return fmt.Errorf("store: intent must have an id and a name")
	}
	data, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("store: encoding intent %s: %w", it.Name, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bucketIntents).Put([]byte(it.ID), data); err != nil {
			return err
		}
		return tx.Bucket(bucketIntentNames).Put([]byte(it.Name), []byte(it.ID))
	})
}

func (s *Store) GetIntentByID(id string) (Intent, error) {
	var it Intent
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucketIntents).Get([]byte(id))
		if data == nil {
			return fmt.Errorf("store: no intent with id %q", id)
		}
		return json.Unmarshal(data, &it)
	})
	return it, err
}

func (s *Store) GetIntentByName(name string) (Intent, error) {
	var id string
	err := s.db.View(func(tx *bbolt.Tx) error {
		idBytes := tx.Bucket(bucketIntentNames).Get([]byte(name))
		if idBytes == nil {
			return fmt.Errorf("store: no intent named %q", name)
		}
		id = string(idBytes)
		return nil
	})
	if err != nil {
		return Intent{}, err
	}
	return s.GetIntentByID(id)
}

// ListIntents returns every intent, sorted by name.
func (s *Store) ListIntents() ([]Intent, error) {
	var out []Intent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketIntents).ForEach(func(_, data []byte) error {
			var it Intent
			if err := json.Unmarshal(data, &it); err != nil {
				return err
			}
			out = append(out, it)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteIntentByID removes the intent record and its name index entry.
// The caller is responsible for deciding what happens to member instances
// (see internal/intent.Manager.Delete's purgeMembers option) — this only
// ever touches the intents bucket, never the instances bucket.
func (s *Store) DeleteIntentByID(id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketIntents)
		data := bucket.Get([]byte(id))
		if data == nil {
			return fmt.Errorf("store: no intent with id %q", id)
		}
		var it Intent
		if err := json.Unmarshal(data, &it); err != nil {
			return err
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		return tx.Bucket(bucketIntentNames).Delete([]byte(it.Name))
	})
}
