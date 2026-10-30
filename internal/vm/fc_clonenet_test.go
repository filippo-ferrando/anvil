//go:build linux

package vm

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
)

type fakeNetnsHelper struct{ created, deleted []string }

func (f *fakeNetnsHelper) CreateNetns(_ context.Context, n hostdev.Netns) error {
	f.created = append(f.created, n.ID)
	return nil
}

func (f *fakeNetnsHelper) DeleteNetns(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCloneSubnet(t *testing.T) {
	if p, err := cloneSubnet(); err != nil || p.String() != defaultCloneSubnet {
		t.Fatalf("default = %s, %v", p, err)
	}
	t.Setenv(cloneSubnetEnv, "10.99.0.7/24")
	if p, err := cloneSubnet(); err != nil || p.String() != "10.99.0.0/24" {
		t.Fatalf("override = %s, %v", p, err)
	}
	for _, bad := range []string{"nope", "8.8.0.0/16", "10.99.0.0/30", "10.254.0.0/16", "10.200.0.0/16", "fd00::/64"} {
		t.Setenv(cloneSubnetEnv, bad)
		if _, err := cloneSubnet(); err == nil {
			t.Errorf("%s=%s accepted", cloneSubnetEnv, bad)
		}
	}
}

func TestCloneAllocate(t *testing.T) {
	var c CloneNetworks
	pool := netip.MustParsePrefix("10.253.0.0/29")
	a, err := c.allocate("eph-A", pool)
	if err != nil || a.String() != "10.253.0.0/30" {
		t.Fatalf("first = %s, %v", a, err)
	}
	b, err := c.allocate("eph-B", pool)
	if err != nil || b.String() != "10.253.0.4/30" {
		t.Fatalf("second = %s, %v", b, err)
	}
	if again, _ := c.allocate("eph-A", pool); again != a {
		t.Errorf("same clone got %s, then %s", a, again)
	}
	if _, err := c.allocate("eph-C", pool); err == nil {
		t.Fatal("a full pool handed out a third link")
	}
	c.free("eph-A")
	if got, err := c.allocate("eph-C", pool); err != nil || got != a {
		t.Errorf("after a free = %s, %v; want %s back", got, err, a)
	}
}

func TestCloneSweep(t *testing.T) {
	hostdev.NetnsDir = t.TempDir()
	t.Cleanup(func() { hostdev.NetnsDir = "/run/netns" })
	for _, name := range []string{"anvil-eph-OLD", "anvil-eph-LIVE", "anvil-other", "eph-NOPREFIX", "cni-1234"} {
		if err := os.WriteFile(filepath.Join(hostdev.NetnsDir, name), nil, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	helper := &fakeNetnsHelper{}
	c := CloneNetworks{Helper: helper, links: map[string]netip.Prefix{"eph-LIVE": netip.MustParsePrefix("10.253.0.0/30")}}
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(helper.deleted, []string{"eph-OLD"}) {
		t.Errorf("Sweep deleted %v, want only eph-OLD", helper.deleted)
	}

	hostdev.NetnsDir = filepath.Join(t.TempDir(), "missing")
	if err := c.Sweep(context.Background()); err != nil {
		t.Errorf("Sweep without a netns dir: %v", err)
	}
}

func TestCloneDNSZonesAndTeardown(t *testing.T) {
	helper := &fakeNetnsHelper{}
	changes := 0
	c := CloneNetworks{Helper: helper, OnChange: func() { changes++ },
		links: map[string]netip.Prefix{"eph-A": netip.MustParsePrefix("10.253.0.8/30")}}
	zones := c.DNSZones()
	if len(zones) != 1 || zones[0].Listen.String() != "10.253.0.9" || zones[0].Subnet.String() != "10.253.0.8/30" {
		t.Fatalf("DNSZones = %+v", zones)
	}
	if err := c.Teardown(context.Background(), "eph-A"); err != nil {
		t.Fatal(err)
	}
	if len(c.DNSZones()) != 0 || changes != 1 || !slices.Equal(helper.deleted, []string{"eph-A"}) {
		t.Errorf("after Teardown: zones %v, %d changes, deleted %v", c.DNSZones(), changes, helper.deleted)
	}
}

func TestCloneNftRules(t *testing.T) {
	rules := cloneNftRules(netip.MustParsePrefix("10.253.0.0/16"))
	for _, want := range []string{
		"delete table ip anvil-eph",
		"ip saddr 10.253.0.0/16 ip daddr != 10.253.0.0/16 masquerade",
		`iifname "anve*" oifname "anve*" drop`,
		`iifname "anve*" oifname "` + FCBridge + `" drop`,
		`oifname "anve*" ct state established,related accept`,
		`iifname "anve*" meta l4proto { tcp, udp } th dport 53 accept`,
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("clone rules lack %q:\n%s", want, rules)
		}
	}
}
