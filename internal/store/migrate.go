package store

import (
	"encoding/json"
	"fmt"
	"strconv"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

var (
	bucketMeta = []byte("meta")   // key -> value
	keySchema  = []byte("schema") // decimal schema version
)

// schemaVersion is the record shape this build writes. Open migrates older databases up to it.
const schemaVersion = 1

// legacyFields holds the instance record fields only older versions wrote.
type legacyFields struct {
	VM *struct {
		MountFS string
	} `json:"vm"`
}

// migrate brings every record up to schemaVersion once, inside tx.
func migrate(tx *bbolt.Tx) error {
	meta, err := tx.CreateBucketIfNotExists(bucketMeta)
	if err != nil {
		return err
	}
	version := 0
	if v := meta.Get(keySchema); v != nil {
		if version, err = strconv.Atoi(string(v)); err != nil {
			return fmt.Errorf("store: bad schema version %q: %w", v, err)
		}
	}
	if version >= schemaVersion {
		return nil
	}
	if err := migrateInstancesV1(tx); err != nil {
		return fmt.Errorf("store: migrating instances: %w", err)
	}
	return meta.Put(keySchema, []byte(strconv.Itoa(schemaVersion)))
}

// migrateInstancesV1 drops records left deleted, turns GUI on, stores each VM's derived
// MAC, and marks VMs whose mounts date from 9p for a fresh cloud-init seed.
func migrateInstancesV1(tx *bbolt.Tx) error {
	bucket := tx.Bucket(bucketInstances)
	names := tx.Bucket(bucketInstanceNames)
	type change struct {
		id   []byte
		data []byte // nil deletes the record
		name string
	}
	var changes []change
	err := bucket.ForEach(func(k, data []byte) error {
		var rec record
		if err := json.Unmarshal(data, &rec); err != nil {
			return fmt.Errorf("decoding instance %s: %w", k, err)
		}
		if rec.State == "deleted" {
			changes = append(changes, change{id: k, name: rec.Name})
			return nil
		}
		var old legacyFields
		if err := json.Unmarshal(data, &old); err != nil {
			return fmt.Errorf("decoding instance %s: %w", k, err)
		}
		rec.GUI = true
		if v := rec.VM; v != nil {
			if v.MACAddress == "" {
				v.MACAddress = instance.DefaultMAC(rec.ID)
			}
			// A 9p-era guest fstab is rewritten by a new seed generation, built on next start.
			legacy9p := old.VM != nil && old.VM.MountFS != "virtiofs"
			if legacy9p && len(v.Mounts) > 0 && v.HypervisorOf() == instance.VMHypervisorQEMU {
				v.Generation++
				v.SeedISOPath = ""
			}
		}
		out, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		changes = append(changes, change{id: k, data: out})
		return nil
	})
	if err != nil {
		return err
	}
	for _, c := range changes {
		id := append([]byte(nil), c.id...)
		if c.data != nil {
			if err := bucket.Put(id, c.data); err != nil {
				return err
			}
			continue
		}
		if err := bucket.Delete(id); err != nil {
			return err
		}
		if string(names.Get([]byte(c.name))) == string(id) {
			if err := names.Delete([]byte(c.name)); err != nil {
				return err
			}
		}
	}
	return nil
}
