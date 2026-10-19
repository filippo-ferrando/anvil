//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

func TestMergeFirecrackerGuest(t *testing.T) {
	base, err := mergeGuestAgent("#cloud-config\n{}\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := mergeFirecrackerGuest(base, true)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseCloudConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	runcmd := doc["runcmd"].([]any)
	if len(runcmd) != 1 || runcmd[0] != fcAgentStartCmd {
		t.Errorf("runcmd = %v, want only the vsock agent start", runcmd)
	}
	if bootcmd := doc["bootcmd"].([]any); len(bootcmd) != 1 || bootcmd[0] != fcPoweroffCmd {
		t.Errorf("bootcmd = %v, want the poweroff-as-reboot link", bootcmd)
	}
	files := doc["write_files"].([]any)
	unit, confd := files[0].(map[string]any)["content"].(string), files[1].(map[string]any)["content"].(string)
	if !strings.Contains(unit, "vsock-listen --path=3:1024") || !strings.Contains(confd, `GA_PATH="3:1024"`) {
		t.Errorf("write_files lack the vsock agent setup: %v", files)
	}

	noAgent, err := mergeFirecrackerGuest("#cloud-config\n{}\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noAgent, "anvil-guest-agent") || !strings.Contains(noAgent, "poweroff.target") {
		t.Errorf("without the agent only the poweroff link belongs here:\n%s", noAgent)
	}
}

// fcTestBackend returns an FCBackend whose catalog has one distro, "fc-test", cached as a
// 64MiB qcow2, plus "qemu-only" which isn't in the Firecracker section.
func fcTestBackend(t *testing.T) *FCBackend {
	t.Helper()
	for _, bin := range []string{"qemu-img", "xorriso"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed, skipping", bin)
		}
	}
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, "instances", id) }
	t.Cleanup(func() { instanceDir = orig })
	origLook := fcLookPath
	fcLookPath = func(string) (string, error) { return "/usr/bin/firecracker", nil }
	t.Cleanup(func() { fcLookPath = origLook })
	t.Setenv(image.KernelEnv, "/test/vmlinux")

	base, err := image.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := base.WithMirrors([]image.MirrorManifest{{ManifestJSON: `{"schema_version": 1,
		"distros": [
			{"id": "fc-test", "arch": "x86_64", "url": "http://unused.invalid/", "default_user": "tester"},
			{"id": "qemu-only", "arch": "x86_64", "url": "http://unused.invalid/"}],
		"firecracker": {"distros": [{"id": "fc-test"}]}}`}})
	if err != nil {
		t.Fatal(err)
	}
	vault := image.NewVault(filepath.Join(root, "prepared"))
	entry, _ := catalog.Find("fc-test", "")
	cached, _ := vault.CachedPath(entry)
	if err := os.MkdirAll(filepath.Dir(cached), 0o750); err != nil {
		t.Fatal(err)
	}
	run(t, "qemu-img", "create", "-f", "qcow2", cached, "64M")
	return NewFCBackend(NewBackend(catalog, vault, noMirrors{}), filepath.Join(root, "firecracker"))
}

func TestFCBackendCreate(t *testing.T) {
	b := fcTestBackend(t)
	ctx := context.Background()

	spec := &instance.Spec{ID: "FC1", Name: "fc1", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "fc-test", DiskGiB: 1, Hypervisor: instance.VMHypervisorFirecracker,
	}}
	if err := b.Create(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	v := spec.VM
	if v.Arch != "x86_64" || v.DefaultUser != "tester" || v.MACAddress == "" {
		t.Errorf("spec after Create = arch %q user %q mac %q", v.Arch, v.DefaultUser, v.MACAddress)
	}
	if info, err := os.Stat(v.DiskPath); err != nil || info.Size() != 1<<30 || filepath.Ext(v.DiskPath) != ".raw" {
		t.Errorf("disk %s = %v, %v; want a 1GiB raw file", v.DiskPath, info, err)
	}
	if _, err := os.Stat(v.SeedISOPath); err != nil {
		t.Errorf("seed: %v", err)
	}

	other := &instance.Spec{ID: "FC2", Name: "fc2", Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "qemu-only", Hypervisor: instance.VMHypervisorFirecracker,
	}}
	if err := b.Create(ctx, other, nil); err == nil || !strings.Contains(err.Error(), "isn't in the firecracker catalog") {
		t.Errorf("Create of a distro missing from the firecracker catalog: err = %v", err)
	}
	if _, err := os.Stat(instanceDir("FC2")); !os.IsNotExist(err) {
		t.Error("a refused Create left an instance dir behind")
	}

	if st, err := b.Status(ctx, spec); err != nil || st != instance.StateStopped {
		t.Errorf("Status of a created VM = %v, %v; want stopped", st, err)
	}
	if err := b.Delete(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(instanceDir("FC1")); !os.IsNotExist(err) {
		t.Error("Delete left the instance dir")
	}
}
