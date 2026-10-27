package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// writeOldDB writes records in the shape older versions left, with no schema version.
func writeOldDB(t *testing.T, path string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	records := map[string]string{
		"01": `{"id":"01","name":"gone","kind":"vm","state":"deleted","vm":{}}`,
		"02": `{"id":"02","name":"web","kind":"vm","state":"stopped","vm":{"SeedISOPath":"/x/seed.iso","Generation":2,` +
			`"Mounts":[{"HostPath":"/h","GuestPath":"/g","Tag":"mount0"}]}}`,
		"03": `{"id":"03","name":"db","kind":"vm","state":"stopped","gui":true,"vm":{"MACAddress":"52:54:00:aa:bb:cc",` +
			`"SeedISOPath":"/y/seed.iso","MountFS":"virtiofs","Mounts":[{"Tag":"mount0"}]}}`,
		"04": `{"id":"04","name":"app","kind":"container","state":"running","container":{}}`,
		"05": `{"id":"05","name":"fc","kind":"vm","state":"stopped","vm":{"Hypervisor":"firecracker","SeedISOPath":"/z/seed.iso",` +
			`"Mounts":[{"Tag":"mount0"}]}}`,
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		ins, err := tx.CreateBucketIfNotExists(bucketInstances)
		if err != nil {
			return err
		}
		names, err := tx.CreateBucketIfNotExists(bucketInstanceNames)
		if err != nil {
			return err
		}
		for id, rec := range records {
			if err := ins.Put([]byte(id), []byte(rec)); err != nil {
				return err
			}
		}
		for name, id := range map[string]string{"gone": "01", "web": "02", "db": "03", "app": "04", "fc": "05"} {
			if err := names.Put([]byte(name), []byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesOldRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anvil.db")
	writeOldDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetByID("01"); err == nil {
		t.Error("expected the deleted record to be removed")
	}
	if _, err := s.GetByName("gone"); err == nil {
		t.Error("expected the deleted record's name to be freed")
	}

	web, err := s.GetByID("02")
	if err != nil {
		t.Fatal(err)
	}
	if !web.GUI || web.VM.MACAddress != instance.DefaultMAC("02") {
		t.Errorf("web: GUI %v, MAC %q", web.GUI, web.VM.MACAddress)
	}
	if web.VM.Generation != 3 || web.VM.SeedISOPath != "" {
		t.Errorf("web: expected a new seed generation for its 9p mounts, got gen %d seed %q", web.VM.Generation, web.VM.SeedISOPath)
	}

	db, _ := s.GetByID("03")
	if db.VM.MACAddress != "52:54:00:aa:bb:cc" || db.VM.Generation != 0 || db.VM.SeedISOPath != "/y/seed.iso" {
		t.Errorf("db: expected stored MAC and seed kept, got %+v", db.VM)
	}
	if app, _ := s.GetByID("04"); !app.GUI || app.Container == nil {
		t.Errorf("app: %+v", app)
	}
	if fc, _ := s.GetByID("05"); fc.VM.Generation != 0 || fc.VM.SeedISOPath != "/z/seed.iso" {
		t.Errorf("fc: expected Firecracker mounts left alone, got %+v", fc.VM)
	}
	s.Close()
}

func TestMigrationRunsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anvil.db")
	writeOldDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A record written in the old shape after the migration must not be touched again.
	err = s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketInstances).Put([]byte("06"), []byte(`{"id":"06","name":"late","kind":"vm","state":"deleted","vm":{}}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	_ = s.db.View(func(tx *bbolt.Tx) error {
		before = append([]byte(nil), tx.Bucket(bucketInstances).Get([]byte("02"))...)
		return nil
	})
	s.Close()

	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetByID("06"); err != nil {
		t.Errorf("expected the second open to leave records alone: %v", err)
	}
	_ = s.db.View(func(tx *bbolt.Tx) error {
		if after := tx.Bucket(bucketInstances).Get([]byte("02")); !bytes.Equal(before, after) {
			t.Errorf("record changed on the second open:\n%s\n%s", before, after)
		}
		return nil
	})
}

func TestOpenFreshDBSetsSchema(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(bucketMeta).Get(keySchema); string(v) != "1" {
			t.Errorf("schema = %q, want 1", v)
		}
		return nil
	})
}
