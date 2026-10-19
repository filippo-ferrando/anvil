//go:build linux

package vm

import (
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
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

var (
	fcNetMu    sync.Mutex
	fcNetReady bool
)

// ensureFCNetwork creates the bridge and its NAT once per daemon run. A failure is
// retried on the next VM start.
func ensureFCNetwork() error {
	fcNetMu.Lock()
	defer fcNetMu.Unlock()
	if fcNetReady {
		return nil
	}
	subnet, gateway, err := fcNetwork()
	if err != nil {
		return err
	}
	if err := ensureBridge(FCBridge, netip.PrefixFrom(gateway, subnet.Bits())); err != nil {
		return err
	}
	if err := runNft(fcNftRules(FCBridge, subnet)); err != nil {
		return err
	}
	allowThroughDocker(FCBridge)
	if data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(data)) != "1" {
		log.Printf("vm: net.ipv4.ip_forward is off, standalone firecracker VMs can't reach outside this host")
	}
	fcNetReady = true
	return nil
}

func ensureBridge(name string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			return fmt.Errorf("vm: finding bridge %s: %w", name, err)
		}
		link = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(link); err != nil {
			return fmt.Errorf("vm: creating bridge %s: %w", name, err)
		}
	}
	nlAddr, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, nlAddr); err != nil {
		return fmt.Errorf("vm: setting %s on bridge %s: %w", addr, name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("vm: bringing up bridge %s: %w", name, err)
	}
	return nil
}

// fcNftRules is anvil's own nftables table: masquerade for traffic leaving the subnet, and
// forwarding to and from the bridge. Recreated whole, so it never piles up rules.
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
	}
}
`, bridge, subnet)
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
// chain Docker keeps for this.
func dockerUserRules(bridge string) [][]string {
	return [][]string{
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
	for _, rule := range dockerUserRules(bridge) {
		if exec.Command("iptables", append([]string{"-C", "DOCKER-USER"}, rule...)...).Run() == nil {
			continue
		}
		if out, err := exec.Command("iptables", append([]string{"-I", "DOCKER-USER"}, rule...)...).CombinedOutput(); err != nil {
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

// allocate picks a free address in subnet for instanceID, as a CIDR.
func (a *fcIPAM) allocate(instanceID string, subnet netip.Prefix) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var taken []string
	if a.instances != nil {
		specs, err := a.instances.List(instance.KindVM)
		if err != nil {
			return "", fmt.Errorf("vm: listing instances for a free address: %w", err)
		}
		for _, s := range specs {
			if s.ID != instanceID && s.VM != nil && s.VM.StaticIP != "" && s.VM.NetworkMode != "bridge" {
				taken = append(taken, stripCIDR(s.VM.StaticIP))
			}
		}
	}
	for id, ip := range a.pending {
		if id != instanceID {
			taken = append(taken, stripCIDR(ip))
		}
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

// release forgets instanceID's address, once it's deleted or its Create failed. Kept until
// then, so a Create racing the registry write can't hand the same address out twice.
func (a *fcIPAM) release(instanceID string) {
	a.mu.Lock()
	delete(a.pending, instanceID)
	a.mu.Unlock()
}

func stripCIDR(s string) string {
	ip, _, _ := strings.Cut(s, "/")
	return ip
}
