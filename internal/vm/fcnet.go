//go:build linux

package vm

import (
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/ipam"
)

// FCBridge is the per-host bridge standalone Firecracker VMs get their tap on, in place of SLIRP.
const FCBridge = "anvil-fc0"

// fcSubnetEnv overrides the standalone Firecracker subnet when the default collides
// with a network the host already uses.
const fcSubnetEnv = "ANVIL_FC_SUBNET"

const defaultFCSubnet = "10.254.254.0/24"

// fcNetwork returns the standalone subnet and its gateway, the bridge's own address.
func fcNetwork() (netip.Prefix, netip.Addr, error) {
	s := os.Getenv(fcSubnetEnv)
	if s == "" {
		s = defaultFCSubnet
	}
	prefix, err := netip.ParsePrefix(s)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("vm: %s=%q must be an IPv4 /24", fcSubnetEnv, s)
	}
	prefix = prefix.Masked()
	return prefix, prefix.Addr().Next(), nil
}

var fcNetMu sync.Mutex

// FCNetworkUp, when set, runs once ensureFCNetwork has created the bridge, so the DNS
// server binds the bridge's gateway before the first guest asks it anything.
var FCNetworkUp func()

// ensureFCNetwork sets up the bridge, its NAT and IPv4 forwarding. Run on every VM start
// (each step is idempotent), so a firewall reload that flushed anvil's table heals.
func ensureFCNetwork() error {
	fcNetMu.Lock()
	created, err := setupFCNetwork()
	fcNetMu.Unlock()
	if created && FCNetworkUp != nil {
		FCNetworkUp()
	}
	return err
}

func setupFCNetwork() (created bool, err error) {
	subnet, gateway, err := fcNetwork()
	if err != nil {
		return false, err
	}
	created, err = ensureBridge(FCBridge, netip.PrefixFrom(gateway, subnet.Bits()))
	if err != nil {
		return created, err
	}
	if err := runNft(fcNftRules(FCBridge, subnet)); err != nil {
		return created, err
	}
	allowThroughDocker(FCBridge)
	// Turned on here rather than by the packages, so hosts without Firecracker VMs don't route.
	const forward = "/proc/sys/net/ipv4/ip_forward"
	if data, err := os.ReadFile(forward); err == nil && strings.TrimSpace(string(data)) != "1" {
		if err := os.WriteFile(forward, []byte("1\n"), 0o644); err != nil {
			log.Printf("vm: net.ipv4.ip_forward is off and turning it on failed (%v), standalone firecracker VMs can't reach outside this host", err)
		}
	}
	return created, nil
}

// ensureBridge brings up bridge name with addr; created reports a bridge that didn't exist.
func ensureBridge(name string, addr netip.Prefix) (created bool, err error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			return false, fmt.Errorf("vm: finding bridge %s: %w", name, err)
		}
		link = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(link); err != nil {
			return false, fmt.Errorf("vm: creating bridge %s: %w", name, err)
		}
		created = true
	}
	nlAddr, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return created, err
	}
	if err := netlink.AddrReplace(link, nlAddr); err != nil {
		return created, fmt.Errorf("vm: setting %s on bridge %s: %w", addr, name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return created, fmt.Errorf("vm: bringing up bridge %s: %w", name, err)
	}
	return created, nil
}

// fcNftRules is anvil's own nftables table: masquerade for traffic leaving the subnet, and
// forwarding from the bridge plus replies to it. Nothing else is routed in: published ports
// go through anvild's proxies. Recreated whole, so it never piles up rules.
func fcNftRules(bridge string, subnet netip.Prefix) string {
	return fmt.Sprintf(`table ip anvil
delete table ip anvil
table ip anvil {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[2]s ip daddr != %[2]s masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%[1]s" accept
		oifname "%[1]s" ct state established,related accept
		oifname "%[1]s" drop
	}
}
`, bridge, subnet)
}

// fcGuestFilter lets tap carry only frames from the guest's own MAC and IPv4 address, so a
// guest can't pose as a neighbour (the NFS server trusts source addresses). Recreated whole.
func fcGuestFilter(instanceID, tap, mac, ip string) string {
	return fmt.Sprintf(`table netdev %[1]s
delete table netdev %[1]s
table netdev %[1]s {
	chain ingress {
		type filter hook ingress device "%[2]s" priority filter; policy drop;
		ether saddr %[3]s ip saddr %[4]s accept
		ether saddr %[3]s arp saddr ether %[3]s arp saddr ip %[4]s accept
	}
}
`, fcGuestTable(instanceID), tap, mac, ip)
}

func fcGuestTable(instanceID string) string { return "anvil-" + strings.ToLower(instanceID) }

// dropGuestFilter removes fcGuestFilter's table, if there is one.
func dropGuestFilter(instanceID string) {
	_ = exec.Command("nft", "delete", "table", "netdev", fcGuestTable(instanceID)).Run()
}

func runNft(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: setting up nftables for %s: %w: %s", FCBridge, err, out)
	}
	return nil
}

// dockerUserRules let the bridge's traffic through Docker's FORWARD drop policy. An accept
// in anvil's own table can't override a drop in Docker's, so they go in DOCKER-USER, the
// chain Docker keeps for this. Traffic towards Docker's own bridges returns to Docker's
// chains, which keep its networks isolated. Listed in chain order.
func dockerUserRules(bridge string) [][]string {
	return [][]string{
		{"-i", bridge, "-o", "docker+", "-j", "RETURN"},
		{"-i", bridge, "-o", "br-+", "-j", "RETURN"},
		{"-i", bridge, "-j", "ACCEPT"},
		{"-o", bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
}

// allowThroughDocker adds dockerUserRules when Docker's DOCKER-USER chain exists. Best-effort:
// without Docker there's nothing to get through.
func allowThroughDocker(bridge string) {
	if exec.Command("iptables", "-n", "-L", "DOCKER-USER").Run() != nil {
		return
	}
	rules := dockerUserRules(bridge)
	// Inserted at the top in reverse, so they end up in order.
	for i := len(rules) - 1; i >= 0; i-- {
		if exec.Command("iptables", append([]string{"-C", "DOCKER-USER"}, rules[i]...)...).Run() == nil {
			continue
		}
		if out, err := exec.Command("iptables", append([]string{"-I", "DOCKER-USER"}, rules[i]...)...).CombinedOutput(); err != nil {
			log.Printf("vm: letting %s through Docker's firewall: %v: %s", bridge, err, out)
		}
	}
}

// FCDNSZone is a forward-only DNS zone on the standalone bridge's gateway, so its guests
// resolve through the host. ok is false until the bridge exists.
func FCDNSZone() (dns.Zone, bool) {
	if _, err := netlink.LinkByName(FCBridge); err != nil {
		return dns.Zone{}, false
	}
	subnet, gateway, err := fcNetwork()
	if err != nil {
		return dns.Zone{}, false
	}
	return dns.Zone{Listen: gateway, Subnet: subnet}, true
}

// Instances lists the instances the daemon knows, for picking free standalone addresses.
type Instances interface {
	List(kind instance.Kind) ([]*instance.Spec, error)
}

// fcIPAM hands out standalone Firecracker addresses. Taken ones come from the registry
// plus the ones handed out to Creates that haven't been saved yet.
type fcIPAM struct {
	instances Instances
	mu        sync.Mutex
	pending   map[string]string // instance ID -> address not in the registry yet
}

// taken lists the standalone addresses other instances than instanceID hold. Called with mu held.
func (a *fcIPAM) taken(instanceID string) ([]string, error) {
	var taken []string
	if a.instances != nil {
		specs, err := a.instances.List(instance.KindVM)
		if err != nil {
			return nil, fmt.Errorf("vm: listing instances for a free address: %w", err)
		}
		for _, s := range specs {
			if s.ID != instanceID && s.VM != nil && s.VM.StaticIP != "" && s.VM.NetworkMode != "bridge" {
				taken = append(taken, ipam.StripCIDR(s.VM.StaticIP))
			}
		}
	}
	for id, ip := range a.pending {
		if id != instanceID {
			taken = append(taken, ipam.StripCIDR(ip))
		}
	}
	return taken, nil
}

// allocate picks a free address in subnet for instanceID, as a CIDR.
func (a *fcIPAM) allocate(instanceID string, subnet netip.Prefix) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	taken, err := a.taken(instanceID)
	if err != nil {
		return "", err
	}
	used, err := ipam.UsedVMIndices(subnet.String(), taken)
	if err != nil {
		return "", err
	}
	idx, err := ipam.NextFreeVMIndex(used) // ponytail: 126 standalone VMs per host, the ipam range
	if err != nil {
		return "", err
	}
	addr, err := ipam.VMAddress(subnet.String(), idx)
	if err != nil {
		return "", err
	}
	if a.pending == nil {
		a.pending = map[string]string{}
	}
	a.pending[instanceID] = addr
	return addr, nil
}

// reserve claims addr (a CIDR) for instanceID, failing when another instance holds it.
func (a *fcIPAM) reserve(instanceID, addr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	taken, err := a.taken(instanceID)
	if err != nil {
		return err
	}
	if slices.Contains(taken, ipam.StripCIDR(addr)) {
		return fmt.Errorf("vm: address %s is already used by another firecracker VM on this host", ipam.StripCIDR(addr))
	}
	if a.pending == nil {
		a.pending = map[string]string{}
	}
	a.pending[instanceID] = addr
	return nil
}

// release forgets instanceID's address, once it's deleted or its Create failed. Kept until
// then, so a Create racing the registry write can't hand the same address out twice.
func (a *fcIPAM) release(instanceID string) {
	a.mu.Lock()
	delete(a.pending, instanceID)
	a.mu.Unlock()
}
