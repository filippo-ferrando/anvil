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

func TestMergeMountsNoMounts(t *testing.T) {
	out, err := mergeMounts("#cloud-config\n{}\n", nil)
	if err != nil {
		t.Fatalf("mergeMounts: %v", err)
	}
	if out != "#cloud-config\n{}\n" {
		t.Errorf("expected mergeMounts to pass through unchanged with no mounts, got %q", out)
	}
}

func TestMergeMountsAddsFstabEntryAndMkdir(t *testing.T) {
	mounts := []instance.Mount{
		{HostPath: "/home/me/project", GuestPath: "/mnt/project", Tag: "mount0"},
	}
	out, err := mergeMounts("#cloud-config\n{}\n", mounts)
	if err != nil {
		t.Fatalf("mergeMounts: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}

	mountsList, ok := doc["mounts"].([]any)
	if !ok || len(mountsList) != 1 {
		t.Fatalf("expected one mounts entry, got %#v", doc["mounts"])
	}
	entry, ok := mountsList[0].([]any)
	if !ok || len(entry) != 6 {
		t.Fatalf("expected a 6-element fstab-shaped entry, got %#v", mountsList[0])
	}
	if entry[0] != "mount0" || entry[1] != "/mnt/project" || entry[2] != "9p" {
		t.Errorf("unexpected mount entry shape: %#v", entry)
	}
	if opts, _ := entry[3].(string); !strings.Contains(opts, "trans=virtio") || !strings.Contains(opts, "rw") {
		t.Errorf("expected virtio 9p rw options, got %q", opts)
	}

	bootcmd, ok := doc["bootcmd"].([]any)
	// Guest path is single-quoted for safe shell interpolation.
	if !ok || len(bootcmd) != 1 || bootcmd[0] != "mkdir -p '/mnt/project'" {
		t.Errorf("expected a bootcmd entry creating the mountpoint, got %#v", doc["bootcmd"])
	}
}

func TestMergeMountsReadOnly(t *testing.T) {
	mounts := []instance.Mount{
		{HostPath: "/home/me/ro", GuestPath: "/mnt/ro", Tag: "mount0", ReadOnly: true},
	}
	out, err := mergeMounts("#cloud-config\n{}\n", mounts)
	if err != nil {
		t.Fatalf("mergeMounts: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	entry := doc["mounts"].([]any)[0].([]any)
	if opts, _ := entry[3].(string); !strings.Contains(opts, ",ro") {
		t.Errorf("expected read-only 9p options, got %q", opts)
	}
}

func TestMergeMountsPreservesExistingBootcmdAndMounts(t *testing.T) {
	existing := "#cloud-config\nbootcmd:\n  - echo existing\nmounts:\n  - [\"existingtag\", \"/mnt/existing\", \"9p\", \"trans=virtio\", \"0\", \"0\"]\n"
	mounts := []instance.Mount{{HostPath: "/x", GuestPath: "/mnt/new", Tag: "mount1"}}
	out, err := mergeMounts(existing, mounts)
	if err != nil {
		t.Fatalf("mergeMounts: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\noutput was:\n%s", err, out)
	}
	if mountsList := doc["mounts"].([]any); len(mountsList) != 2 {
		t.Errorf("expected the pre-existing mount plus the new one, got %#v", mountsList)
	}
	if bootcmd := doc["bootcmd"].([]any); len(bootcmd) != 2 {
		t.Errorf("expected the pre-existing bootcmd plus the new mkdir, got %#v", bootcmd)
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
