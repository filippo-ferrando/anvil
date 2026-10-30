package hostdev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
)

// The network layout inside every clone netns. Clones of one warm snapshot share the
// guest's MAC and address, so each gets its own netns with this same layout.
const (
	CloneTap     = "tap0"          // the guest's tap, owned by anvild's user
	CloneTapAddr = "10.200.0.1/30" // the guest's gateway and DNS server
	CloneGuestIP = "10.200.0.2"
	// CloneTapMAC is fixed too: a restored guest still has it in its ARP cache.
	CloneTapMAC   = "06:00:0a:c8:00:01"
	cloneVethPeer = "veth0" // the netns end of the veth pair
)

// NetnsDir holds the named network namespaces, as `ip netns` keeps them.
var NetnsDir = "/run/netns"

// NetnsPrefix starts the name of every clone netns under NetnsDir.
const NetnsPrefix = "anvil-"

var cloneIDRe = regexp.MustCompile(`^eph-[A-Za-z0-9]{1,40}$`)

// Netns asks for the network namespace of one clone. Link is the clone's own /30: its
// first address stays on the host end of the veth pair, its second goes in the netns.
type Netns struct {
	ID   string
	Link string
}

// NetnsPath is the file the netns of clone id is pinned at, the path the jailer's --netns takes.
func NetnsPath(id string) string { return filepath.Join(NetnsDir, NetnsPrefix+id) }

// CloneVeth is the host end of clone id's veth pair, short enough for a 15-byte link name.
func CloneVeth(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "anve" + hex.EncodeToString(sum[:5])
}

// CloneLink splits a clone /30 into the host end's address and the netns end's.
func CloneLink(link string) (host, peer netip.Prefix, err error) {
	p, err := netip.ParsePrefix(link)
	if err != nil || !p.Addr().Is4() || p.Bits() != 30 || p.Masked() != p || !p.Addr().IsPrivate() {
		return host, peer, fmt.Errorf("invalid clone link %q, want a private IPv4 /30", link)
	}
	host = netip.PrefixFrom(p.Addr().Next(), 30)
	peer = netip.PrefixFrom(host.Addr().Next(), 30)
	return host, peer, nil
}

func (n Netns) validate() error {
	if !cloneIDRe.MatchString(n.ID) {
		return fmt.Errorf("invalid clone id %q", n.ID)
	}
	if _, _, err := CloneLink(n.Link); err != nil {
		return err
	}
	if inside, _ := netip.ParsePrefix(CloneTapAddr); inside.Overlaps(netip.MustParsePrefix(n.Link)) {
		return fmt.Errorf("clone link %s overlaps the in-netns layout %s", n.Link, CloneTapAddr)
	}
	return nil
}

// CreateNetns sets up clone n's netns: the veth pair, the guest's tap and the in-netns NAT.
// A netns left by an earlier run under the same id is replaced.
func (c Client) CreateNetns(ctx context.Context, n Netns) error {
	_, err := c.call(ctx, request{Op: "netns-create", Netns: &n})
	return err
}

// DeleteNetns removes clone id's netns and veth pair; a missing one is not an error.
func (c Client) DeleteNetns(ctx context.Context, id string) error {
	_, err := c.call(ctx, request{Op: "netns-delete", Netns: &Netns{ID: id}})
	return err
}
