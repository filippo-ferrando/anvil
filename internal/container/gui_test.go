package container

import (
	"os"
	"path/filepath"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
)

func withGUIPaths(t *testing.T, dri string) string {
	t.Helper()
	base := t.TempDir()
	oldRelay, oldDri := guiRelayDir, driDir
	guiRelayDir = func(id string) string { return filepath.Join(base, id) }
	driDir = dri
	t.Cleanup(func() { guiRelayDir, driDir = oldRelay, oldDri })
	return base
}

func TestAddGUIParams_BindsRelayDirs(t *testing.T) {
	base := withGUIPaths(t, t.TempDir()) // no device files: no GPU
	var p docker.CreateContainerParams
	addGUIParams(&p, "01")

	want := []docker.VolumeMount{
		{HostPath: filepath.Join(base, "01"), ContainerPath: gui.ContainerRuntimeDir},
		{HostPath: filepath.Join(base, "01", "x11"), ContainerPath: gui.ContainerX11Dir},
	}
	if len(p.Volumes) != 2 || p.Volumes[0] != want[0] || p.Volumes[1] != want[1] {
		t.Fatalf("volumes %+v", p.Volumes)
	}
	if p.Devices != nil || p.GroupAdd != nil {
		t.Fatalf("no GPU expected, got %v %v", p.Devices, p.GroupAdd)
	}
}

func TestAddGUIParams_PassesDevicesWithTheirGroups(t *testing.T) {
	withGUIPaths(t, "/dev") // has device files on any Linux host
	var p docker.CreateContainerParams
	addGUIParams(&p, "01")
	if len(p.Devices) != 1 || p.Devices[0] != "/dev" {
		t.Fatalf("devices %v", p.Devices)
	}
	if len(p.GroupAdd) == 0 {
		t.Fatal("expected the device files' groups")
	}
	seen := map[string]bool{}
	for _, g := range p.GroupAdd {
		if seen[g] {
			t.Fatalf("duplicate group %s in %v", g, p.GroupAdd)
		}
		seen[g] = true
	}
}

func TestEnsureGUIDirs(t *testing.T) {
	base := withGUIPaths(t, "")
	created, err := ensureGUIDirs("01")
	if err != nil || !created {
		t.Fatalf("first call: %v %v", created, err)
	}
	for _, dir := range []string{filepath.Join(base, "01"), filepath.Join(base, "01", "x11")} {
		st, err := os.Stat(dir)
		if err != nil || st.Mode().Perm() != 0o777 {
			t.Fatalf("%s: %v %v", dir, st.Mode(), err)
		}
	}
	if created, err := ensureGUIDirs("01"); err != nil || created {
		t.Fatalf("second call: %v %v", created, err)
	}
	removeGUIDirs("01")
	if _, err := os.Stat(filepath.Join(base, "01")); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
}
