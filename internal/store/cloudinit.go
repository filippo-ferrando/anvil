package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

var (
	bucketCloudInit = []byte("cloud_init_configs") // name -> json(docRecord)
	bucketManifests = []byte("manifests")          // name -> json(docRecord)
)

// Doc is one named text document in a library bucket: a cloud-init config or an intent manifest.
type Doc struct {
	Name       string
	Content    string
	ModifiedAt time.Time
}

// CloudInitConfig is one saved cloud-init user-data document, referenced by
// name from VMSpec.CloudInitName.
type CloudInitConfig = Doc

type docRecord struct {
	Content    string    `json:"content"`
	ModifiedAt time.Time `json:"modified_at"`
}

// SaveCloudInit creates or overwrites the named config.
func (s *Store) SaveCloudInit(name, content string) error {
	return s.saveDoc(bucketCloudInit, "cloud-init config", name, content)
}

func (s *Store) GetCloudInit(name string) (CloudInitConfig, error) {
	return s.getDoc(bucketCloudInit, "cloud-init config", name)
}

// ListCloudInit returns every saved config, sorted by name.
func (s *Store) ListCloudInit() ([]CloudInitConfig, error) { return s.listDocs(bucketCloudInit) }

func (s *Store) RenameCloudInit(oldName, newName string) error {
	return s.renameDoc(bucketCloudInit, "cloud-init config", oldName, newName)
}

func (s *Store) DeleteCloudInit(name string) error {
	return s.deleteDoc(bucketCloudInit, "cloud-init config", name)
}

// SaveManifest creates or overwrites the named intent manifest.
func (s *Store) SaveManifest(name, content string) error {
	return s.saveDoc(bucketManifests, "manifest", name, content)
}

func (s *Store) GetManifest(name string) (Doc, error) {
	return s.getDoc(bucketManifests, "manifest", name)
}

// ListManifests returns every saved manifest, sorted by name.
func (s *Store) ListManifests() ([]Doc, error) { return s.listDocs(bucketManifests) }

func (s *Store) RenameManifest(oldName, newName string) error {
	return s.renameDoc(bucketManifests, "manifest", oldName, newName)
}

func (s *Store) DeleteManifest(name string) error {
	return s.deleteDoc(bucketManifests, "manifest", name)
}

func (s *Store) saveDoc(bucket []byte, kind, name, content string) error {
	if name == "" {
		return fmt.Errorf("store: %s name must not be empty", kind)
	}
	data, err := json.Marshal(docRecord{Content: content, ModifiedAt: time.Now()})
	if err != nil {
		return fmt.Errorf("store: encoding %s %s: %w", kind, name, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(name), data)
	})
}

func (s *Store) getDoc(bucket []byte, kind, name string) (Doc, error) {
	var rec docRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucket).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("store: no %s named %q: %w", kind, name, instance.ErrNotFound)
		}
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return Doc{}, err
	}
	return Doc{Name: name, Content: rec.Content, ModifiedAt: rec.ModifiedAt}, nil
}

func (s *Store) listDocs(bucket []byte) ([]Doc, error) {
	var out []Doc
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(k, data []byte) error {
			var rec docRecord
			if err := json.Unmarshal(data, &rec); err != nil {
				return err
			}
			out = append(out, Doc{Name: string(k), Content: rec.Content, ModifiedAt: rec.ModifiedAt})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) renameDoc(bucket []byte, kind, oldName, newName string) error {
	if newName == "" {
		return fmt.Errorf("store: new %s name must not be empty", kind)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		data := b.Get([]byte(oldName))
		if data == nil {
			return fmt.Errorf("store: no %s named %q: %w", kind, oldName, instance.ErrNotFound)
		}
		if b.Get([]byte(newName)) != nil {
			return fmt.Errorf("store: a %s named %q already exists", kind, newName)
		}
		if err := b.Put([]byte(newName), data); err != nil {
			return err
		}
		return b.Delete([]byte(oldName))
	})
}

func (s *Store) deleteDoc(bucket []byte, kind, name string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		if b.Get([]byte(name)) == nil {
			return fmt.Errorf("store: no %s named %q: %w", kind, name, instance.ErrNotFound)
		}
		return b.Delete([]byte(name))
	})
}
