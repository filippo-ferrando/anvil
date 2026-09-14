package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/anvil-project/anvil/internal/instance"
)

var bucketCloudInit = []byte("cloud_init_configs") // name -> json(cloudInitRecord)

// CloudInitConfig is one saved cloud-init user-data document, referenced by
// name from VMSpec.CloudInitName.
type CloudInitConfig struct {
	Name       string
	Content    string
	ModifiedAt time.Time
}

type cloudInitRecord struct {
	Content    string    `json:"content"`
	ModifiedAt time.Time `json:"modified_at"`
}

// SaveCloudInit creates or overwrites the named config.
func (s *Store) SaveCloudInit(name, content string) error {
	if name == "" {
		return fmt.Errorf("store: cloud-init config name must not be empty")
	}
	rec := cloudInitRecord{Content: content, ModifiedAt: time.Now()}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encoding cloud-init config %s: %w", name, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketCloudInit).Put([]byte(name), data)
	})
}

func (s *Store) GetCloudInit(name string) (CloudInitConfig, error) {
	var rec cloudInitRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucketCloudInit).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("store: no cloud-init config named %q: %w", name, instance.ErrNotFound)
		}
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return CloudInitConfig{}, err
	}
	return CloudInitConfig{Name: name, Content: rec.Content, ModifiedAt: rec.ModifiedAt}, nil
}

// ListCloudInit returns every saved config, sorted by name.
func (s *Store) ListCloudInit() ([]CloudInitConfig, error) {
	var out []CloudInitConfig
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketCloudInit).ForEach(func(k, data []byte) error {
			var rec cloudInitRecord
			if err := json.Unmarshal(data, &rec); err != nil {
				return err
			}
			out = append(out, CloudInitConfig{Name: string(k), Content: rec.Content, ModifiedAt: rec.ModifiedAt})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) RenameCloudInit(oldName, newName string) error {
	if newName == "" {
		return fmt.Errorf("store: new cloud-init config name must not be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCloudInit)
		data := bucket.Get([]byte(oldName))
		if data == nil {
			return fmt.Errorf("store: no cloud-init config named %q: %w", oldName, instance.ErrNotFound)
		}
		if existing := bucket.Get([]byte(newName)); existing != nil {
			return fmt.Errorf("store: a cloud-init config named %q already exists", newName)
		}
		if err := bucket.Put([]byte(newName), data); err != nil {
			return err
		}
		return bucket.Delete([]byte(oldName))
	})
}

func (s *Store) DeleteCloudInit(name string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCloudInit)
		if bucket.Get([]byte(name)) == nil {
			return fmt.Errorf("store: no cloud-init config named %q: %w", name, instance.ErrNotFound)
		}
		return bucket.Delete([]byte(name))
	})
}
