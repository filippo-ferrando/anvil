package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// touch writes a small file at path, making its dir first.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestImageServerFirecrackerCache(t *testing.T) {
	dir := t.TempDir()
	vault := image.NewVault(filepath.Join(dir, "prepared"))
	vault.RawDir = filepath.Join(dir, "firecracker")
	fc := &image.FCCache{Dir: vault.RawDir, Vault: vault}
	touch(t, filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2"))
	touch(t, filepath.Join(fc.Dir, "debian-13-x86_64.raw"))
	touch(t, filepath.Join(fc.Dir, "ubuntu-24.04-x86_64.raw")) // its base is gone already
	touch(t, filepath.Join(fc.Dir, "anvil-fc-kernel-6.18.57-x86_64"))
	touch(t, filepath.Join(fc.Dir, "gui-tools.iso"))
	srv := NewImageServer(testStore(t), vault, nil, nil, fc)

	list, err := srv.List(t.Context(), &anvilv1.ImageListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, img := range list.GetImages() {
		kinds[img.GetKind()+" "+img.GetId()+" "+img.GetArch()] = img.GetPath()
	}
	for _, want := range []string{
		"base debian-13 x86_64", "firecracker-raw debian-13 x86_64", "firecracker-raw ubuntu-24.04 x86_64",
		"firecracker-kernel anvil-fc-kernel-6.18.57-x86_64 x86_64",
	} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("list lacks %q: %v", want, kinds)
		}
	}
	if len(kinds) != 4 {
		t.Errorf("list has %d entries, want 4 (no GUI tools ISO): %v", len(kinds), kinds)
	}

	// A raw copy on its own, by kind: its base stays.
	if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: "debian-13", Kind: "firecracker-raw"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2")); err != nil {
		t.Fatalf("deleting the raw copy took the base: %v", err)
	}
	touch(t, filepath.Join(fc.Dir, "debian-13-x86_64.raw"))
	// The base, by default: its raw copy goes with it.
	if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: "debian-13"}); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2"), filepath.Join(fc.Dir, "debian-13-x86_64.raw")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still there after deleting the base", gone)
		}
	}
	// A leftover raw copy and the kernel go by id alone.
	for _, id := range []string{"ubuntu-24.04", "anvil-fc-kernel-6.18.57-x86_64"} {
		if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: id}); err != nil {
			t.Errorf("delete %s: %v", id, err)
		}
	}
	if left, _ := fc.List(); len(left) != 0 {
		t.Errorf("left in the firecracker cache: %+v", left)
	}
}

// A Firecracker disk is a full copy with no backing file; its base still counts as in use.
func TestUsersOfFirecracker(t *testing.T) {
	dir := t.TempDir()
	vault := image.NewVault(filepath.Join(dir, "prepared"))
	base := filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2")
	touch(t, base)
	disk := filepath.Join(dir, "disk.raw")
	touch(t, disk)
	fcVM := func(name, ref string) *instance.Spec {
		return &instance.Spec{Name: name, VM: &instance.VMSpec{ImageRef: ref, Arch: "x86_64", DiskPath: disk,
			Hypervisor: instance.VMHypervisorFirecracker}}
	}
	specs := []*instance.Spec{fcVM("a", "debian-13"), fcVM("b", "ubuntu-24.04"), {Name: "c", VM: &instance.VMSpec{ImageRef: "debian-13", DiskPath: disk}}}
	bases := vmBases{}
	bases.fill(specs, vault)
	if got := usersOf(base, specs, bases); len(got) != 1 || got[0] != "a" {
		t.Fatalf("usersOf = %v, want [a]", got)
	}
	// The raw copy a Firecracker disk was made from counts too.
	if got := usersOf(filepath.Join(dir, "firecracker", "debian-13-x86_64.raw"), specs, bases); len(got) != 1 || got[0] != "a" {
		t.Fatalf("usersOf raw = %v, want [a]", got)
	}
	// Every Firecracker VM of the kernel's arch boots from it.
	if got := usersOf(filepath.Join(dir, "firecracker", "anvil-fc-kernel-6.18.57-x86_64"), specs, bases); len(got) != 2 {
		t.Fatalf("usersOf kernel = %v, want [a b]", got)
	}
}

func TestImageDeleteWaitsForCreates(t *testing.T) {
	dir := t.TempDir()
	vault := image.NewVault(filepath.Join(dir, "prepared"))
	var leases []func()
	vault.Keep = func(ctx context.Context, release func()) bool {
		leases = append(leases, release)
		return true
	}
	base := filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2")
	other := filepath.Join(vault.PreparedDir, "ubuntu-24.04-x86_64.qcow2")
	touch(t, base)
	touch(t, other)
	srv := NewImageServer(testStore(t), vault, nil, nil, nil)
	vault.Use(t.Context(), base) // a create of debian-13 in flight

	if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: "debian-13"}); !errors.Is(err, image.ErrInUse) {
		t.Fatalf("expected delete to be refused while an instance is being created from it, got %v", err)
	}
	if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: "ubuntu-24.04"}); err != nil {
		t.Fatalf("an image no create uses should delete, got %v", err)
	}
	leases[0]()
	if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: "debian-13"}); err != nil {
		t.Fatalf("expected delete once the create is saved, got %v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Error("the base is still there after delete")
	}
}

func TestImageDeleteForceIgnoresCreates(t *testing.T) {
	dir := t.TempDir()
	vault := image.NewVault(filepath.Join(dir, "prepared"))
	vault.Keep = func(context.Context, func()) bool { return true }
	base := filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2")
	touch(t, base)
	srv := NewImageServer(testStore(t), vault, nil, nil, nil)
	vault.Use(t.Context(), base)

	if _, err := srv.Delete(t.Context(), &anvilv1.ImageDeleteRequest{Id: "debian-13", Force: true}); err != nil {
		t.Fatalf("expected --force to delete anyway, got %v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Error("the base is still there after a forced delete")
	}
}

// Bases resolved before the vault lock are reused; only new VMs get looked up.
func TestUsersOfLockedReusesBases(t *testing.T) {
	dir := t.TempDir()
	vault := image.NewVault(filepath.Join(dir, "prepared"))
	base := filepath.Join(vault.PreparedDir, "debian-13-x86_64.qcow2")
	touch(t, base)
	fcVM := func(id string) *instance.Spec {
		return &instance.Spec{ID: id, Name: id, VM: &instance.VMSpec{ImageRef: "debian-13", Arch: "x86_64", DiskPath: base,
			Hypervisor: instance.VMHypervisorFirecracker}}
	}
	// "a" was resolved earlier to another base, so the cached answer must win.
	bases := vmBases{"a": "/elsewhere.qcow2"}
	list := func() ([]*instance.Spec, error) { return []*instance.Spec{fcVM("a"), fcVM("b")}, nil }
	users, err := usersOfLocked(base, list, bases, vault)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0] != "b" {
		t.Fatalf("users = %v, want [b]", users)
	}
	if bases["b"] != base {
		t.Fatalf("bases[b] = %q, want %q", bases["b"], base)
	}
}
