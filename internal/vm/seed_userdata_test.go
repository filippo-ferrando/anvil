//go:build linux

package vm

import (
	"encoding/json"
	"os"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

const richUserData = `#cloud-config
# a comment that is lost once anvil re-encodes the document
hostname: box
packages:
  - htop
  - qemu-guest-agent
runcmd:
  - echo hi
  - [sh, -c, "echo list form"]
bootcmd:
  - echo early
write_files:
  - path: /etc/motd
    permissions: "0644"
    content: |
      line one
      line two
ssh_authorized_keys:
  - ssh-rsa AAAAOLD old@example
users:
  - name: dev
    groups: [wheel]
    uid: 1500
    lock_passwd: false
ratio: 1.50
when: 2001-12-14
quoted: "123"
`

// seedCases are VM specs and user-data whose seed user-data is pinned in testdata/seed-userdata.json.
func seedCases() map[string]struct {
	userData string
	v        instance.VMSpec
} {
	keys := []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE test@example", "ssh-rsa AAAAB second@example"}
	mounts := []instance.Mount{{Tag: "mount0", GuestPath: "/mnt/a"}, {Tag: "mount1", GuestPath: "/srv/my data", ReadOnly: true}}
	hosts := map[string]string{"web": "10.55.201.2", "db": "10.55.201.3"}
	fc := instance.VMHypervisorFirecracker
	type c = struct {
		userData string
		v        instance.VMSpec
	}
	return map[string]c{
		"empty-nothing":       {"", instance.VMSpec{NoGuestAgent: true}},
		"verbatim-nothing":    {richUserData, instance.VMSpec{NoGuestAgent: true}},
		"empty-agent":         {"", instance.VMSpec{}},
		"keys-only":           {"", instance.VMSpec{SSHPublicKeys: keys, NoGuestAgent: true}},
		"rich-everything":     {richUserData, instance.VMSpec{SSHPublicKeys: keys, Mounts: mounts, Generation: 2, ExtraHosts: hosts}},
		"rich-fc-everything":  {richUserData, instance.VMSpec{Hypervisor: fc, Gateway: "10.254.254.1", SSHPublicKeys: keys, Mounts: mounts, Generation: 2, ExtraHosts: hosts}},
		"fc-no-agent":         {"", instance.VMSpec{Hypervisor: fc, NoGuestAgent: true}},
		"fc-rich-no-agent":    {richUserData, instance.VMSpec{Hypervisor: fc, NoGuestAgent: true, Generation: 1}},
		"generation-only":     {"#cloud-config\n{}\n", instance.VMSpec{Generation: 4, NoGuestAgent: true}},
		"hosts-only":          {"#cloud-config\nbootcmd:\n  - echo existing\n", instance.VMSpec{ExtraHosts: hosts, NoGuestAgent: true}},
		"rich-agent-only":     {richUserData, instance.VMSpec{}},
		"null-doc-with-keys":  {"#cloud-config\n", instance.VMSpec{SSHPublicKeys: keys}},
		"no-header-with-keys": {"packages: [curl]\n", instance.VMSpec{SSHPublicKeys: keys}},
	}
}

// The merged user-data must stay byte-identical to what earlier releases produced.
func TestSeedUserDataGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/seed-userdata.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	cases := seedCases()
	if len(cases) != len(want) {
		t.Fatalf("%d cases, %d golden outputs", len(cases), len(want))
	}
	for name, c := range cases {
		v := c.v
		got, err := seedUserData(&v, c.userData)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if got != want[name] {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", name, got, want[name])
		}
	}
}
