package intent

import (
	"context"
	"net/netip"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// launchingInstances records the params each Launch receives and reports
// back a spec with a fixed ID.
type launchingInstances struct {
	fakeInstances
	got []instance.LaunchParams
}

func (f *launchingInstances) Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	f.got = append(f.got, params)
	spec := &instance.Spec{ID: "id-" + params.Name, Name: params.Name, VM: params.VM, Container: params.Container}
	if params.VM != nil {
		spec.Kind = instance.KindVM
	} else {
		spec.Kind = instance.KindContainer
	}
	progress(instance.LaunchEvent{Instance: spec})
	return nil
}

// countingDNS counts Refresh calls and records the zones seen on each.
type countingDNS struct {
	mgr   *Manager
	calls int
	seen  [][]string
}

func (d *countingDNS) Refresh(ctx context.Context) error {
	d.calls++
	zones, err := d.mgr.DNSZones(ctx)
	if err != nil {
		return err
	}
	var domains []string
	for _, z := range zones {
		domains = append(domains, z.Domain)
	}
	d.seen = append(d.seen, domains)
	return nil
}

func TestLaunchPointsMembersAtIntentDNS(t *testing.T) {
	inst := &launchingInstances{}
	m := NewManager(newFakeStore(), inst, newFakeNetworker())
	d := &countingDNS{mgr: m}
	m.DNS = d

	err := m.Launch(t.Context(), instance.LaunchParams{
		Name: "myapp-db", IntentName: "myapp", Role: "db", VM: &instance.VMSpec{},
	}, func(instance.LaunchEvent) {})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	it, _ := m.Store.GetIntentByName("myapp")
	vm := inst.got[0].VM
	if len(vm.DNSServers) != 1 || vm.DNSServers[0] != it.Network.Gateway {
		t.Errorf("expected the VM's DNS server to be the gateway %s, got %v", it.Network.Gateway, vm.DNSServers)
	}
	if len(vm.DNSSearch) != 1 || vm.DNSSearch[0] != "myapp.anvil" {
		t.Errorf("expected search domain myapp.anvil, got %v", vm.DNSSearch)
	}
	// The zone must already be served while the first member launches,
	// before the intent is in the store.
	if d.calls < 2 || len(d.seen[0]) != 1 || d.seen[0][0] != "myapp.anvil" {
		t.Errorf("expected the new intent's zone during launch, got calls=%d seen=%v", d.calls, d.seen)
	}

	err = m.Launch(t.Context(), instance.LaunchParams{
		Name: "myapp-cache", IntentName: "myapp", Role: "cache", Container: &instance.ContainerSpec{},
	}, func(instance.LaunchEvent) {})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	c := inst.got[1].Container
	if len(c.DNSServers) != 1 || c.DNSServers[0] != it.Network.Gateway || len(c.DNSSearch) != 1 {
		t.Errorf("expected the container to use the intent DNS, got servers=%v search=%v", c.DNSServers, c.DNSSearch)
	}
}

func TestLaunchWithoutDNSLeavesResolverUnset(t *testing.T) {
	inst := &launchingInstances{}
	m := NewManager(newFakeStore(), inst, newFakeNetworker())
	err := m.Launch(t.Context(), instance.LaunchParams{
		Name: "myapp-db", IntentName: "myapp", Role: "db", VM: &instance.VMSpec{},
	}, func(instance.LaunchEvent) {})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if inst.got[0].VM.DNSServers != nil {
		t.Errorf("expected no DNS servers without a DNS hook, got %v", inst.got[0].VM.DNSServers)
	}
}

// addrNetworker reports fixed live container addresses.
type addrNetworker struct {
	fakeNetworker
	addrs map[string]string
}

func (n *addrNetworker) ContainerAddress(ctx context.Context, networkName, containerID string) (string, error) {
	return n.addrs[containerID], nil
}

func TestDNSZonesRecordsRolesAndNames(t *testing.T) {
	s := newFakeStore()
	_ = s.PutIntent(store.Intent{
		ID:   "intent1",
		Name: "MyApp",
		Members: []store.IntentMember{
			{InstanceID: "vm1", Role: "db", Kind: instance.KindVM, IP: "10.55.201.2"},
			{InstanceID: "ct1", Role: "cache", Kind: instance.KindContainer, IP: "10.55.201.130"},
		},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1", Subnet: "10.55.201.0/24", Gateway: "10.55.201.1"},
	})
	_ = s.PutIntent(store.Intent{ID: "intent2", Name: "nonet"})

	inst := &fakeInstances{specs: map[string]*instance.Spec{
		"vm1": {ID: "vm1", Name: "myapp-db", Kind: instance.KindVM},
		"ct1": {ID: "ct1", Name: "myapp-cache", Kind: instance.KindContainer, Container: &instance.ContainerSpec{ContainerID: "c0ffee"}},
	}}
	net := &addrNetworker{fakeNetworker: *newFakeNetworker(), addrs: map[string]string{"c0ffee": "10.55.201.131"}}
	m := NewManager(s, inst, net)

	zones, err := m.DNSZones(t.Context())
	if err != nil {
		t.Fatalf("DNSZones: %v", err)
	}
	if len(zones) != 1 {
		t.Fatalf("expected one zone (the intent without a network is skipped), got %d", len(zones))
	}
	z := zones[0]
	if z.Domain != "myapp.anvil" || z.Listen != netip.MustParseAddr("10.55.201.1") {
		t.Errorf("unexpected zone %+v", z)
	}
	want := map[string]string{
		"db":          "10.55.201.2",
		"myapp-db":    "10.55.201.2",
		"cache":       "10.55.201.131", // the live address wins over the stored one
		"myapp-cache": "10.55.201.131",
	}
	if len(z.Records) != len(want) {
		t.Errorf("expected %d records, got %v", len(want), z.Records)
	}
	for label, addr := range want {
		if got := z.Records[label]; got != netip.MustParseAddr(addr) {
			t.Errorf("record %s = %v, want %s", label, got, addr)
		}
	}
}

func TestMemberDNSName(t *testing.T) {
	if got := MemberDNSName("myapp", "Web_1"); got != "web-1.myapp.anvil" {
		t.Errorf("MemberDNSName = %q", got)
	}
}
