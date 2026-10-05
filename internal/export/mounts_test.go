package export

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// fakeInstances answers Export's lookups with one fixed spec and records the
// stop/start calls it makes around the archiving.
type fakeInstances struct {
	spec    *instance.Spec
	stopped bool
	started bool
}

func (f *fakeInstances) Info(names []string) ([]*instance.Spec, error) {
	if len(names) == 1 && names[0] == f.spec.Name {
		return []*instance.Spec{f.spec}, nil
	}
	return nil, instance.ErrNotFound
}
func (f *fakeInstances) GetByID(string) (*instance.Spec, error) { return f.spec, nil }
func (f *fakeInstances) Start(context.Context, []string) error  { f.started = true; return nil }
func (f *fakeInstances) Stop(context.Context, []string, bool, time.Duration) error {
	f.stopped = true
	return nil
}
func (f *fakeInstances) Launch(context.Context, instance.LaunchParams, func(instance.LaunchEvent)) error {
	return nil
}

type fakeStore struct{}

func (fakeStore) GetIntentByName(string) (store.Intent, error) {
	return store.Intent{}, instance.ErrNotFound
}
func (fakeStore) GetCloudInit(string) (store.CloudInitConfig, error) {
	return store.CloudInitConfig{}, nil
}

type fakeVMImporter struct{}

func (fakeVMImporter) BaseImageChecksum(*instance.Spec) (string, error) { return "", nil }
func (fakeVMImporter) PrepareImportedDisk(context.Context, string, string, string, string) error {
	return nil
}

// TestExportArchivesVMMounts covers a VM's shared folders travelling inside the
// bundle, with their guest paths and virtiofs tags.
func TestExportArchivesVMMounts(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed, skipping")
	}
	dir := t.TempDir()

	shared := filepath.Join(dir, "shared")
	if err := os.MkdirAll(shared, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "notes.txt"), []byte("keep me"), 0o640); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(diskPath, []byte("fake qcow2"), 0o640); err != nil {
		t.Fatal(err)
	}

	spec := &instance.Spec{
		ID:   "01JTEST",
		Name: "web",
		Kind: instance.KindVM,
		VM: &instance.VMSpec{
			ImageRef: "test-distro", Arch: "x86_64", CPUs: 2, MemoryMiB: 2048,
			DiskPath: diskPath,
			Mounts: []instance.Mount{
				{HostPath: shared, GuestPath: "/data", Tag: "mount0"},
				{HostPath: filepath.Join(dir, "gone"), GuestPath: "/missing", Tag: "mount1"},
			},
		},
	}
	instances := &fakeInstances{spec: spec}
	m := NewManager(fakeStore{}, instances, nil, fakeVMImporter{})

	bundle := filepath.Join(dir, "web.tar.zst")
	if err := m.Export(context.Background(), ExportParams{Name: "web", OutputPath: bundle}, func(string) {}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !instances.stopped {
		t.Error("expected the instance to be stopped for a consistent archive")
	}

	out := filepath.Join(dir, "extracted")
	if err := extractTarZst(context.Background(), bundle, out); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}

	mounts := manifest.Members[0].VM.Mounts
	if len(mounts) != 2 {
		t.Fatalf("manifest records %d mount(s), want 2", len(mounts))
	}
	if mounts[0].GuestPath != "/data" || mounts[0].Tag != "mount0" {
		t.Errorf("unexpected first mount: %+v", mounts[0])
	}
	if mounts[0].Archive != fmt.Sprintf("mounts/%s/0", spec.ID) {
		t.Errorf("first mount archived at %q", mounts[0].Archive)
	}
	// A host path that can't be read is still recorded, just without contents.
	if mounts[1].GuestPath != "/missing" || mounts[1].Archive != "" {
		t.Errorf("unexpected second mount: %+v", mounts[1])
	}

	data, err := os.ReadFile(filepath.Join(out, mounts[0].Archive, "notes.txt"))
	if err != nil {
		t.Fatalf("reading the archived folder: %v", err)
	}
	if string(data) != "keep me" {
		t.Errorf("archived contents are %q, want %q", data, "keep me")
	}
}
