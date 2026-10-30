//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
)

// cloneSubnetEnv overrides the pool clone links come from, when the default collides
// with a network the host already uses.
const cloneSubnetEnv = "ANVIL_EPH_SUBNET"

const defaultCloneSubnet = "10.253.0.0/16"

// cloneVethGlob matches the host end of every clone's veth pair (hostdev.CloneVeth).
const cloneVethGlob = "anve*"

// NetnsHelper builds clone network namespaces as root; hostdev.Client asks the root helper.
type NetnsHelper interface {
	CreateNetns(ctx context.Context, n hostdev.Netns) error
	DeleteNetns(ctx context.Context, id string) error
}

// CloneNet is the network of one ephemeral clone: its own netns, NATed out through a /30.
type CloneNet struct {
	NetNS string     // the netns path, the jailer's --netns
	Tap   string     // the guest's tap inside it
	Host  netip.Addr // the host end of the link, where the guest's DNS goes
}

// CloneNetworks hands out clone networks. Clones live only in anvild's memory, so a link
// is free again once its clone is torn down, and Sweep clears what a crash left.
type CloneNetworks struct {
	Helper   NetnsHelper
	OnChange func() // runs when the DNS zones change

	mu    sync.Mutex
	links map[string]netip.Prefix // clone ID -> its /30
}

// cloneSubnet returns the pool of clone links, checked against the other fixed subnets.
func cloneSubnet() (netip.Prefix, error) {
	s := os.Getenv(cloneSubnetEnv)
	if s == "" {
		s = defaultCloneSubnet
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p.Bits() > 29 {
		return netip.Prefix{}, fmt.Errorf("vm: %s=%q must be a private IPv4 subnet of /29 or more", cloneSubnetEnv, s)
	}
	p = p.Masked()
	fc, _, err := fcNetwork()
	if err != nil {
		return netip.Prefix{}, err
	}
	if p.Overlaps(fc) || p.Overlaps(netip.MustParsePrefix(hostdev.CloneTapAddr)) {
		return netip.Prefix{}, fmt.Errorf("vm: %s=%s overlaps %s or %s", cloneSubnetEnv, p, fc, hostdev.CloneTapAddr)
	}
	return p, nil
}

// allocate picks the first /30 of pool no clone holds, for id.
func (c *CloneNetworks) allocate(id string, pool netip.Prefix) (netip.Prefix, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.links == nil {
		c.links = map[string]netip.Prefix{}
	}
	if link, ok := c.links[id]; ok {
		return link, nil
	}
	used := make(map[netip.Addr]bool, len(c.links))
	for _, l := range c.links {
		used[l.Addr()] = true
	}
	for a := pool.Addr(); pool.Contains(a); {
		if !used[a] {
			c.links[id] = netip.PrefixFrom(a, 30)
			return c.links[id], nil
		}
		for range 4 {
			a = a.Next()
		}
	}
	return netip.Prefix{}, fmt.Errorf("vm: no free clone link left in %s (%d clones running)", pool, len(c.links))
}

func (c *CloneNetworks) free(id string) {
	c.mu.Lock()
	delete(c.links, id)
	c.mu.Unlock()
}

// Setup gives clone id its netns, link and host NAT.
func (c *CloneNetworks) Setup(ctx context.Context, id string) (_ CloneNet, err error) {
	pool, err := cloneSubnet()
	if err != nil {
		return CloneNet{}, err
	}
	link, err := c.allocate(id, pool)
	if err != nil {
		return CloneNet{}, err
	}
	defer func() {
		if err != nil {
			_ = c.Helper.DeleteNetns(context.WithoutCancel(ctx), id)
			c.free(id)
		}
	}()
	if err := ensureCloneNAT(pool); err != nil {
		return CloneNet{}, err
	}
	if err := c.Helper.CreateNetns(ctx, hostdev.Netns{ID: id, Link: link.String()}); err != nil {
		return CloneNet{}, fmt.Errorf("vm: setting up the network of clone %s: %w", id, err)
	}
	host, _, _ := hostdev.CloneLink(link.String())
	if err := hostEndUp(hostdev.CloneVeth(id), host); err != nil {
		return CloneNet{}, err
	}
	c.changed()
	return CloneNet{NetNS: hostdev.NetnsPath(id), Tap: hostdev.CloneTap, Host: host.Addr()}, nil
}

// Teardown removes clone id's network, once its firecracker is gone.
func (c *CloneNetworks) Teardown(ctx context.Context, id string) error {
	err := c.Helper.DeleteNetns(ctx, id)
	c.free(id)
	c.changed()
	if err != nil {
		return fmt.Errorf("vm: removing the network of clone %s: %w", id, err)
	}
	return nil
}

// Sweep removes clone netns no clone of this anvild holds, the ones an earlier one left.
func (c *CloneNetworks) Sweep(ctx context.Context) error {
	entries, err := os.ReadDir(hostdev.NetnsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Name(), hostdev.NetnsPrefix)
		if !ok || !strings.HasPrefix(id, "eph-") {
			continue
		}
		c.mu.Lock()
		_, live := c.links[id]
		c.mu.Unlock()
		if live {
			continue
		}
		log.Printf("vm: removing the network of leftover clone %s", id)
		if err := c.Helper.DeleteNetns(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DNSZones is one forward-only zone per clone, on the host end of its link.
func (c *CloneNetworks) DNSZones() []dns.Zone {
	c.mu.Lock()
	defer c.mu.Unlock()
	zones := make([]dns.Zone, 0, len(c.links))
	for _, link := range c.links {
		host, _, _ := hostdev.CloneLink(link.String())
		zones = append(zones, dns.Zone{Listen: host.Addr(), Subnet: link})
	}
	return zones
}

func (c *CloneNetworks) changed() {
	if c.OnChange != nil {
		c.OnChange()
	}
}

// hostEndUp gives the host end of a clone's veth pair its address.
func hostEndUp(name string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("vm: finding clone veth %s: %w", name, err)
	}
	nlAddr, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, nlAddr); err != nil {
		return fmt.Errorf("vm: setting %s on %s: %w", addr, name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("vm: bringing up %s: %w", name, err)
	}
	return nil
}

// ensureCloneNAT loads the host's clone table and turns on forwarding. Run on every Setup
// (both are idempotent), so a firewall reload that flushed the table heals.
func ensureCloneNAT(pool netip.Prefix) error {
	if err := runNft(cloneNftRules(pool)); err != nil {
		return err
	}
	allowThroughDocker(strings.TrimSuffix(cloneVethGlob, "*") + "+") // iptables spells the glob "+"
	const forward = "/proc/sys/net/ipv4/ip_forward"
	if data, err := os.ReadFile(forward); err == nil && strings.TrimSpace(string(data)) != "1" {
		if err := os.WriteFile(forward, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("vm: net.ipv4.ip_forward is off and turning it on failed: %w", err)
		}
	}
	return nil
}

// cloneNftRules is the host's clone table: clones reach outside through masquerade, but not
// each other, anvil's other networks or the host beyond DNS and ping on their own link.
func cloneNftRules(pool netip.Prefix) string {
	return fmt.Sprintf(`table ip anvil-eph
delete table ip anvil-eph
table ip anvil-eph {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[2]s ip daddr != %[2]s masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%[1]s" oifname "%[1]s" drop
		iifname "%[1]s" oifname "%[3]s" drop
		iifname "%[1]s" oifname "docker*" drop
		iifname "%[1]s" oifname "br-*" drop
		iifname "%[1]s" accept
		oifname "%[1]s" ct state established,related accept
		oifname "%[1]s" drop
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[1]s" ct state established,related accept
		# Only the host end of the clone's own link, not other clones' or the host's addresses.
		iifname "%[1]s" fib daddr . iif type != local drop
		iifname "%[1]s" meta l4proto { tcp, udp } th dport 53 accept
		iifname "%[1]s" icmp type echo-request accept
		iifname "%[1]s" drop
	}
}
`, cloneVethGlob, pool, FCBridge)
}
