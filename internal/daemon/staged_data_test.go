package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
)

func TestAdoptOneMovesStagedData(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "anvil-migrate-data-01J-0")
	if err := os.MkdirAll(staged, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "a.txt"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}

	mnt := instance.Mount{HostPath: "/srv/old", GuestPath: "/data", SourceDataPath: staged}
	dest := filepath.Join(dir, "adopted")
	if err := adoptOne(&mnt.SourceDataPath, &mnt.HostPath, dest); err != nil {
		t.Fatalf("adoptOne: %v", err)
	}
	if mnt.HostPath != dest {
		t.Errorf("host path is %q, want %q", mnt.HostPath, dest)
	}
	if mnt.SourceDataPath != "" {
		t.Error("expected the staged path to be cleared once adopted")
	}
	if got, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(got) != "data" {
		t.Errorf("adopted contents are %q, %v", got, err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("expected the staging directory to be gone")
	}
}

// TestAdoptStagedDataLeavesOrdinaryLaunchesAlone makes sure a launch that
// carries no staged data keeps the host paths it was given.
func TestAdoptStagedDataLeavesOrdinaryLaunchesAlone(t *testing.T) {
	params := instance.LaunchParams{
		Name:      "web",
		VM:        &instance.VMSpec{Mounts: []instance.Mount{{HostPath: "/srv/data", GuestPath: "/data"}}},
		Container: &instance.ContainerSpec{Volumes: []instance.VolumeMount{{HostPath: "/srv/db", ContainerPath: "/db"}}},
	}
	if err := adoptStagedData(&params); err != nil {
		t.Fatalf("adoptStagedData: %v", err)
	}
	if params.VM.Mounts[0].HostPath != "/srv/data" || params.Container.Volumes[0].HostPath != "/srv/db" {
		t.Error("expected host paths to be left as they were")
	}
}

// TestAdoptOneRejectsArbitraryPath makes sure a caller can't point a launch at
// a path outside a migration upload: adopting one deletes it.
func TestAdoptOneRejectsArbitraryPath(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "etc")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	mnt := instance.Mount{GuestPath: "/data", SourceDataPath: victim}
	if err := adoptOne(&mnt.SourceDataPath, &mnt.HostPath, filepath.Join(dir, "dest")); err == nil {
		t.Fatal("expected a path outside a staged upload to be rejected")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the rejected path should be untouched: %v", err)
	}
}
