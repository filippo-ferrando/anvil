//go:build linux

package vm

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/anvil-project/anvil/internal/instance"
)

func TestMergeSSHKeysNoExistingDataNoKeys(t *testing.T) {
	out, err := mergeSSHKeys("", nil)
	if err != nil {
		t.Fatalf("mergeSSHKeys: %v", err)
	}
	if out != "#cloud-config\n{}\n" {
		t.Errorf("expected the empty default, got %q", out)
	}
}

func TestMergeSSHKeysNoExistingDataWithKeys(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE test@example"
	out, err := mergeSSHKeys("", []string{key})
	if err != nil {
		t.Fatalf("mergeSSHKeys: %v", err)
	}
	assertValidCloudConfigWithKey(t, out, key)
}

func TestMergeSSHKeysWithExistingCloudConfig(t *testing.T) {
	existing := "#cloud-config\npackages:\n  - htop\n  - curl\n"
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE test@example"
	out, err := mergeSSHKeys(existing, []string{key})
	if err != nil {
		t.Fatalf("mergeSSHKeys: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	packages, _ := doc["packages"].([]any)
	if len(packages) != 2 {
		t.Errorf("expected the existing packages list to survive the merge, got %v", doc["packages"])
	}
	assertValidCloudConfigWithKey(t, out, key)
}

func TestMergeSSHKeysAppendsToExistingKeyList(t *testing.T) {
	existing := "#cloud-config\nssh_authorized_keys:\n  - ssh-rsa AAAAOLD old@example\n"
	newKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE test@example"
	out, err := mergeSSHKeys(existing, []string{newKey})
	if err != nil {
		t.Fatalf("mergeSSHKeys: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	keys, _ := doc["ssh_authorized_keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("expected both the old and new key to be present, got %v", doc["ssh_authorized_keys"])
	}
}

func TestMergeExtraHostsNoHosts(t *testing.T) {
	out, err := mergeExtraHosts("#cloud-config\n{}\n", nil)
	if err != nil {
		t.Fatalf("mergeExtraHosts: %v", err)
	}
	if out != "#cloud-config\n{}\n" {
		t.Errorf("expected mergeExtraHosts to pass through unchanged with no hosts, got %q", out)
	}
}

func TestMergeExtraHostsAddsGuardedBootcmdLines(t *testing.T) {
	hosts := map[string]string{"web": "10.55.201.2", "db": "10.55.201.3"}
	out, err := mergeExtraHosts("#cloud-config\n{}\n", hosts)
	if err != nil {
		t.Fatalf("mergeExtraHosts: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	bootcmd, ok := doc["bootcmd"].([]any)
	if !ok || len(bootcmd) != 2 {
		t.Fatalf("expected one bootcmd entry per host, got %#v", doc["bootcmd"])
	}
	joined := strings.Join(toStrings(t, bootcmd), "\n")
	for _, want := range []string{"10.55.201.2 web", "10.55.201.3 db"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a bootcmd line containing %q, got:\n%s", want, joined)
		}
		if !strings.Contains(joined, "grep -qxF") {
			t.Errorf("expected an idempotency guard (grep -qxF) in the bootcmd lines, got:\n%s", joined)
		}
	}
}

func TestMergeExtraHostsIsDeterministic(t *testing.T) {
	hosts := map[string]string{"web": "10.55.201.2", "db": "10.55.201.3", "cache": "10.55.201.4"}
	first, err := mergeExtraHosts("#cloud-config\n{}\n", hosts)
	if err != nil {
		t.Fatalf("mergeExtraHosts: %v", err)
	}
	second, err := mergeExtraHosts("#cloud-config\n{}\n", hosts)
	if err != nil {
		t.Fatalf("mergeExtraHosts: %v", err)
	}
	if first != second {
		t.Errorf("expected the same input map to always produce identical output (sorted by name), got:\n%s\nvs\n%s", first, second)
	}
}

func TestMergeExtraHostsPreservesExistingBootcmd(t *testing.T) {
	existing := "#cloud-config\nbootcmd:\n  - echo existing\n"
	out, err := mergeExtraHosts(existing, map[string]string{"web": "10.55.201.2"})
	if err != nil {
		t.Fatalf("mergeExtraHosts: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	if bootcmd := doc["bootcmd"].([]any); len(bootcmd) != 2 {
		t.Errorf("expected the pre-existing bootcmd plus the new hosts entry, got %#v", bootcmd)
	}
}

func toStrings(t *testing.T, items []any) []string {
	t.Helper()
	out := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("expected a string bootcmd entry, got %#v", item)
		}
		out[i] = s
	}
	return out
}

// assertValidCloudConfigWithKey parses out as YAML and checks key is
// present in ssh_authorized_keys.
func assertValidCloudConfigWithKey(t *testing.T, out, key string) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	keys, ok := doc["ssh_authorized_keys"].([]any)
	if !ok {
		t.Fatalf("expected ssh_authorized_keys to be a list, got %#v (full doc: %#v)", doc["ssh_authorized_keys"], doc)
	}
	found := false
	for _, k := range keys {
		if k == key {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in ssh_authorized_keys, got %v", key, keys)
	}
}

func stripHeader(userData string) string {
	return strings.TrimPrefix(userData, "#cloud-config\n")
}

func TestCheckSnapshotName(t *testing.T) {
	for _, name := range []string{"before-upgrade", "snap_1", "v1.2.3", "A"} {
		if err := checkSnapshotName(name); err != nil {
			t.Errorf("checkSnapshotName(%q): unexpected error: %v", name, err)
		}
	}
	// A name that could confuse a shell arg or an HMP "savevm <name>"
	// command line (built by plain string concatenation) must be rejected.
	for _, name := range []string{"", "has space", "a/b", "a;b", "a\tb"} {
		if err := checkSnapshotName(name); err == nil {
			t.Errorf("checkSnapshotName(%q): expected an error", name)
		}
	}
}

func TestBridgeNetworkConfigNameservers(t *testing.T) {
	v := &instance.VMSpec{
		NetworkMode: "bridge",
		StaticIP:    "10.55.201.2/24",
		Gateway:     "10.55.201.1",
		DNSServers:  []string{"10.55.201.1"},
		DNSSearch:   []string{"myapp.anvil"},
	}
	cfg := bridgeNetworkConfig(v, "52:54:00:aa:bb:cc")
	var doc struct {
		Network struct {
			Ethernets map[string]struct {
				Nameservers struct {
					Addresses []string `yaml:"addresses"`
					Search    []string `yaml:"search"`
				} `yaml:"nameservers"`
			} `yaml:"ethernets"`
		} `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("network-config isn't valid YAML: %v\n%s", err, cfg)
	}
	ns := doc.Network.Ethernets["anvil0"].Nameservers
	if len(ns.Addresses) != 1 || ns.Addresses[0] != "10.55.201.1" || len(ns.Search) != 1 || ns.Search[0] != "myapp.anvil" {
		t.Errorf("unexpected nameservers %+v in\n%s", ns, cfg)
	}

	v.DNSServers, v.DNSSearch = nil, nil
	if strings.Contains(bridgeNetworkConfig(v, "52:54:00:aa:bb:cc"), "nameservers") {
		t.Error("expected no nameservers block without DNS servers")
	}
}

func TestAllocateFreePortUniqueAcrossCalls(t *testing.T) {
	seen := map[int]bool{}
	for range 50 {
		p, err := allocateFreePort()
		if err != nil {
			t.Fatalf("allocateFreePort: %v", err)
		}
		if seen[p] {
			t.Fatalf("port %d handed out twice", p)
		}
		seen[p] = true
	}
}
