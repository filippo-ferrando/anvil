package export

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// fakeIntents launches members until failOn, and records the undo calls.
type fakeIntents struct {
	failOn   string
	deleted  []string
	launched []string
}

func (f *fakeIntents) Launch(_ context.Context, p instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	if p.Name == f.failOn {
		return errors.New("boot failed")
	}
	f.launched = append(f.launched, p.Name)
	progress(instance.LaunchEvent{Instance: &instance.Spec{ID: "id-" + p.Name, Name: p.Name}})
	return nil
}
func (f *fakeIntents) Remove(context.Context, string, string) (store.Intent, error) {
	return store.Intent{}, nil
}
func (f *fakeIntents) Delete(_ context.Context, name string, _ bool) (store.Intent, error) {
	f.deleted = append(f.deleted, name)
	return store.Intent{}, nil
}

// A member failing to launch takes back the members launched before it, and the new intent.
func TestImportRollsBackEarlierMembers(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed, skipping")
	}
	probe, err := os.MkdirTemp(config.ExportStagingDir(), "probe-*")
	if err != nil {
		t.Skipf("import stages under %s, not writable here: %v", config.ExportStagingDir(), err)
	}
	_ = os.Remove(probe)

	manifest, _ := json.Marshal(Manifest{IntentName: "shop", Members: []Member{
		{Name: "shop-web", Kind: "container", Role: "web", Container: &Container{ImageRef: "nginx"}},
		{Name: "shop-db", Kind: "container", Role: "db", Container: &Container{ImageRef: "postgres"}},
	}})
	bundle := filepath.Join(t.TempDir(), "shop.tar.zst")
	if err := writeTarZst(context.Background(), bundle, func(tw *tar.Writer) error {
		return addBytesToTar(tw, "manifest.json", manifest)
	}); err != nil {
		t.Fatal(err)
	}

	instances := &fakeInstances{}
	intents := &fakeIntents{failOn: "shop-db"}
	m := NewManager(fakeStore{}, instances, intents, fakeVMImporter{})
	if _, err := m.Import(context.Background(), ImportParams{BundlePath: bundle}, func(string) {}); err == nil {
		t.Fatal("expected the import to fail")
	}
	if len(instances.deleted) != 1 || instances.deleted[0] != "shop-web" {
		t.Errorf("deleted %v, want the member launched before the failure", instances.deleted)
	}
	if len(intents.deleted) != 1 || intents.deleted[0] != "shop" {
		t.Errorf("intents deleted %v, want the intent the import made", intents.deleted)
	}
}

// A failed export leaves an existing bundle as it was, and no temp file behind.
func TestWriteTarZstKeepsOldBundleOnFailure(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed, skipping")
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "b.tar.zst")
	if err := os.WriteFile(bundle, []byte("old bundle"), 0o640); err != nil {
		t.Fatal(err)
	}
	err := writeTarZst(context.Background(), bundle, func(tw *tar.Writer) error {
		_ = addBytesToTar(tw, "manifest.json", []byte("{}"))
		return errors.New("disk read failed")
	})
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if got, _ := os.ReadFile(bundle); string(got) != "old bundle" {
		t.Errorf("existing bundle is now %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}

	if err := writeTarZst(context.Background(), bundle, func(tw *tar.Writer) error {
		return addBytesToTar(tw, "manifest.json", []byte("{}"))
	}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(bundle); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("new bundle: %v, %v", info, err)
	}
}

// The rollback runs where the import test above can't stage its bundle.
func TestUndoImport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	launched := []*instance.Spec{{ID: "id-web", Name: "shop-web"}}

	instances := &fakeInstances{}
	intents := &fakeIntents{}
	m := NewManager(fakeStore{}, instances, intents, fakeVMImporter{})
	m.undoImport(context.Background(), "shop", false, launched, []string{dir}, func(string) {})
	if len(instances.deleted) != 1 || len(intents.deleted) != 1 {
		t.Errorf("deleted instances %v, intents %v", instances.deleted, intents.deleted)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("restored folder left behind")
	}

	// An intent that was there before the import is kept.
	intents = &fakeIntents{}
	m = NewManager(fakeStore{}, &fakeInstances{}, intents, fakeVMImporter{})
	m.undoImport(context.Background(), "shop", true, launched, nil, func(string) {})
	if len(intents.deleted) != 0 {
		t.Errorf("an existing intent was deleted: %v", intents.deleted)
	}
}
