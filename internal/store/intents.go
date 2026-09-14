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

// IntentMember is one instance's membership in an Intent, referenced by ID.
type IntentMember struct {
	InstanceID string
	Role       string
	Kind       instance.Kind

	// IP is this member's address on the intent's shared network, empty if
	// the intent has no network.
	IP string
}

// IntentNetwork is the shared bridge network created for one intent's
// members, populated once on first member launch.
type IntentNetwork struct {
	EngineNetworkName string // Docker/Podman network name (== "anvil-"+Intent.ID)
	BridgeInterface   string // the underlying Linux bridge's interface name, explicitly requested at create time
	Subnet            string // CIDR, e.g. "10.55.201.0/24"
	Gateway           string
	DockerIPRange     string // CIDR sub-range reserved for the engine's own IPAM, see internal/intent/ipam
}

// Intent is a named group of VM/container instances sharing one network.
type Intent struct {
	ID      string
	Name    string
	Members []IntentMember
	Network *IntentNetwork
}

// PutIntent inserts or replaces it by ID, keeping the name->id index in sync.
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
			return fmt.Errorf("store: no intent with id %q: %w", id, instance.ErrNotFound)
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
			return fmt.Errorf("store: no intent named %q: %w", name, instance.ErrNotFound)
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
// It never touches the instances bucket.
func (s *Store) DeleteIntentByID(id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketIntents)
		data := bucket.Get([]byte(id))
		if data == nil {
			return fmt.Errorf("store: no intent with id %q: %w", id, instance.ErrNotFound)
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
