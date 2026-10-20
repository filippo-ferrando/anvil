package export

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// exportManifest exports spec on its own and returns the bundle's manifest and path.
func exportManifest(t *testing.T, spec *instance.Spec) (Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(fakeStore{}, &fakeInstances{spec: spec}, nil, fakeVMImporter{})
	bundle := filepath.Join(dir, spec.Name+".tar.zst")
	if err := m.Export(context.Background(), ExportParams{Name: spec.Name, OutputPath: bundle}, func(string) {}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	out := filepath.Join(dir, "extracted")
	if err := extractTarZst(context.Background(), bundle, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, bundle
}

func vmWithDisk(t *testing.T, hv instance.VMHypervisor) *instance.Spec {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "disk.qcow2")
	if err := os.WriteFile(disk, []byte("fake qcow2"), 0o640); err != nil {
		t.Fatal(err)
	}
	return &instance.Spec{ID: "01JFC", Name: "web", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "test-distro", Arch: "x86_64", DiskPath: disk, StaticIP: "10.254.254.9/24", Hypervisor: hv,
	}}
}

// A standalone Firecracker guest's address is configured inside its disk, so it travels;
// a standalone QEMU VM's never does.
func TestExportRecordsFirecrackerAddress(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed, skipping")
	}
	fc, _ := exportManifest(t, vmWithDisk(t, instance.VMHypervisorFirecracker))
	if mem := fc.Members[0]; mem.VM.Hypervisor != "firecracker" || mem.StaticIP != "10.254.254.9/24" {
		t.Errorf("firecracker member: hypervisor %q, address %q", mem.VM.Hypervisor, mem.StaticIP)
	}
	q, _ := exportManifest(t, vmWithDisk(t, ""))
	if mem := q.Members[0]; mem.VM.Hypervisor != "qemu" || mem.StaticIP != "" {
		t.Errorf("qemu member: hypervisor %q, address %q", mem.VM.Hypervisor, mem.StaticIP)
	}
}

func TestFillLaunchParamsFirecracker(t *testing.T) {
	staging := t.TempDir()
	m := NewManager(fakeStore{}, &fakeInstances{}, nil, fakeVMImporter{})
	mem := Member{Name: "web", Kind: "vm", StaticIP: "10.254.254.9/24",
		VM: &VM{ImageRef: "test-distro", Arch: "x86_64", DiskFile: "disks/01JFC.qcow2", Hypervisor: "firecracker"}}

	var standalone instance.LaunchParams
	if err := m.fillLaunchParams(context.Background(), &standalone, mem, "web", staging, new([]string), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if v := standalone.VM; v.Hypervisor != instance.VMHypervisorFirecracker || v.StaticIP != "10.254.254.9/24" {
		t.Errorf("standalone: hypervisor %q, address %q", v.Hypervisor, v.StaticIP)
	}

	// An intent member's address goes through the intent instead.
	member := instance.LaunchParams{IntentName: "app"}
	if err := m.fillLaunchParams(context.Background(), &member, mem, "web", staging, new([]string), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if v := member.VM; v.Hypervisor != instance.VMHypervisorFirecracker || v.StaticIP != "" {
		t.Errorf("intent member: hypervisor %q, address %q", v.Hypervisor, v.StaticIP)
	}
}

// An unknown hypervisor fails right after the manifest is read, before any disk moves.
func TestImportRejectsUnknownHypervisor(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed, skipping")
	}
	probe, err := os.MkdirTemp(config.ExportStagingDir(), "probe-*")
	if err != nil {
		t.Skipf("import stages under %s, not writable here: %v", config.ExportStagingDir(), err)
	}
	_ = os.Remove(probe)
	spec := vmWithDisk(t, "bogus")
	_, bundle := exportManifest(t, spec)
	instances := &fakeInstances{spec: spec}
	m := NewManager(fakeStore{}, instances, nil, fakeVMImporter{})
	_, err = m.Import(context.Background(), ImportParams{BundlePath: bundle}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "unknown hypervisor") {
		t.Fatalf("expected an unknown hypervisor error, got %v", err)
	}
	if instances.launched != 0 {
		t.Error("nothing should be launched")
	}
}
