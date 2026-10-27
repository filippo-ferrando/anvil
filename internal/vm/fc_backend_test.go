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

func TestAddFirecrackerGuest(t *testing.T) {
	out := applyTo(t, "#cloud-config\n{}\n", func(doc map[string]any) {
		addGuestAgent(doc)
		addFirecrackerGuest(doc, true)
	})
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

	noAgent := applyTo(t, "#cloud-config\n{}\n", func(doc map[string]any) { addFirecrackerGuest(doc, false) })
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
	t.Setenv(fcJailerEnv, "off")

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
	return NewFCBackend(NewBackend(catalog, vault, noMirrors{}), filepath.Join(root, "firecracker"), nil)
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
	if v.StaticIP != "10.254.254.2/24" || v.Gateway != "10.254.254.1" || len(v.DNSServers) != 1 || v.DNSServers[0] != v.Gateway {
		t.Errorf("standalone network = ip %q gw %q dns %v", v.StaticIP, v.Gateway, v.DNSServers)
	}
	// The seed's network-config pins that address to the VM's MAC.
	if cfg := bridgeNetworkConfig(v, v.MACAddress); !strings.Contains(cfg, "10.254.254.2/24") || !strings.Contains(cfg, v.MACAddress) {
		t.Errorf("network-config:\n%s", cfg)
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

func TestFCIPAM(t *testing.T) {
	subnet, _, err := fcNetwork()
	if err != nil {
		t.Fatal(err)
	}
	existing := fakeInstances{
		{ID: "A", VM: &instance.VMSpec{StaticIP: "10.254.254.2/24"}},
		// An intent member's address is on another bridge and doesn't count.
		{ID: "B", VM: &instance.VMSpec{StaticIP: "10.254.254.3/24", NetworkMode: "bridge"}},
	}
	a := fcIPAM{instances: existing}
	first, err := a.allocate("C", subnet)
	if err != nil || first != "10.254.254.3/24" {
		t.Fatalf("first = %q, %v", first, err)
	}
	// Not saved yet, but still taken for the next Create.
	if second, _ := a.allocate("D", subnet); second != "10.254.254.4/24" {
		t.Fatalf("second = %q, want the next address", second)
	}
	if again, _ := a.allocate("C", subnet); again != first {
		t.Fatalf("asking again for C = %q, want its own %q", again, first)
	}
	a.release("C")
	if reused, _ := a.allocate("E", subnet); reused != first {
		t.Fatalf("after release = %q, want %q back", reused, first)
	}

	t.Setenv(fcSubnetEnv, "192.168.77.9/24")
	if subnet, gw, err := fcNetwork(); err != nil || subnet.String() != "192.168.77.0/24" || gw.String() != "192.168.77.1" {
		t.Fatalf("override = %v %v %v", subnet, gw, err)
	}
	t.Setenv(fcSubnetEnv, "10.0.0.0/16")
	if _, _, err := fcNetwork(); err == nil {
		t.Fatal("expected only a /24 to be accepted")
	}
}

type fakeInstances []*instance.Spec

func (f fakeInstances) List(instance.Kind) ([]*instance.Spec, error) { return f, nil }

func TestFCNetworkRules(t *testing.T) {
	subnet, _, _ := fcNetwork()
	rules := fcNftRules(FCBridge, subnet)
	for _, want := range []string{
		"delete table ip anvil",
		"ip saddr 10.254.254.0/24 ip daddr != 10.254.254.0/24 masquerade",
		`iifname "anvil-fc0" accept`,
		`oifname "anvil-fc0" ct state established,related accept`,
		`oifname "anvil-fc0" drop`,
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("nft rules lack %q:\n%s", want, rules)
		}
	}
	if nft, err := exec.LookPath("nft"); err == nil {
		// nft -c only parses and checks; it changes nothing.
		cmd := exec.Command(nft, "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(rules)
		if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "Operation not permitted") {
			t.Errorf("nft -c rejects the rules: %v: %s", err, out)
		}
	}
	// Docker's own bridges go back to Docker's isolation before the bridge's catch-all accept.
	r := dockerUserRules(FCBridge)
	if len(r) != 4 || strings.Join(r[0], " ") != "-i anvil-fc0 -o docker+ -j RETURN" ||
		strings.Join(r[2], " ") != "-i anvil-fc0 -j ACCEPT" {
		t.Errorf("DOCKER-USER rules = %v", r)
	}
}

func TestFCGuestFilter(t *testing.T) {
	rules := fcGuestFilter("01JX", "tap-01jx", "52:54:00:12:34:56", "10.254.254.7")
	for _, want := range []string{
		`table netdev anvil-01jx`, `hook ingress device "tap-01jx"`, `policy drop`,
		`ether saddr 52:54:00:12:34:56 ip saddr 10.254.254.7 accept`,
		`arp saddr ip 10.254.254.7 accept`,
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("guest filter lacks %q:\n%s", want, rules)
		}
	}
}
