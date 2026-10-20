package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/peer"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// asMe is a request ctx from the test's own uid, the daemon's user here.
func asMe() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: callerAddr{uid: uint32(os.Getuid())}})
}

func TestAdoptOneMovesStagedData(t *testing.T) {
	dir := t.TempDir()
	// Only the real staging dir is accepted, so this test stages there.
	staged, err := os.MkdirTemp("/var/tmp", "anvil-migrate-data-test-")
	if err != nil {
		t.Skipf("no writable /var/tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(staged) })
	if err := os.WriteFile(filepath.Join(staged, "a.txt"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}

	mnt := instance.Mount{HostPath: "/srv/old", GuestPath: "/data", SourceDataPath: staged}
	dest := filepath.Join(dir, "adopted")
	if err := adoptOne(asMe(), &mnt.SourceDataPath, &mnt.HostPath, dest); err != nil {
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
	if _, err := adoptStagedData(asMe(), &params); err != nil {
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
	if err := adoptOne(asMe(), &mnt.SourceDataPath, &mnt.HostPath, filepath.Join(dir, "dest")); err == nil {
		t.Fatal("expected a path outside a staged upload to be rejected")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the rejected path should be untouched: %v", err)
	}
}

// TestAdoptOneRejectsSymlink makes sure a staged name pointing at the daemon's own
// files isn't followed.
func TestAdoptOneRejectsSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join("/var/tmp", "anvil-migrate-data-test-link-"+filepath.Base(target))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no writable /var/tmp: %v", err)
	}
	t.Cleanup(func() { os.Remove(link) })
	mnt := instance.Mount{GuestPath: "/data", SourceDataPath: link}
	if err := adoptOne(asMe(), &mnt.SourceDataPath, &mnt.HostPath, filepath.Join(t.TempDir(), "dest")); err == nil {
		t.Fatal("expected a symlinked staging path to be rejected")
	}
}
