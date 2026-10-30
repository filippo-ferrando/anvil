//go:build linux

package hostdev

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// LocalNetns builds clone netns in this process instead of through the helper, for a
// caller that already runs as root (tests under unshare).
type LocalNetns struct{ UID, GID int }

func (l LocalNetns) CreateNetns(_ context.Context, n Netns) error {
	return createNetns(n, l.UID, l.GID)
}
func (LocalNetns) DeleteNetns(_ context.Context, id string) error { return deleteNetns(id) }

// createNetns builds clone n's netns as CreateNetns describes, the guest's tap owned by uid:gid.
func createNetns(n Netns, uid, gid int) (err error) {
	if err := n.validate(); err != nil {
		return err
	}
	hostAddr, peerAddr, _ := CloneLink(n.Link)
	_ = deleteNetns(n.ID) // left by a crash; nothing runs in it now
	defer func() {
		if err != nil {
			_ = deleteNetns(n.ID)
		}
	}()

	ns, err := pinNewNetns(NetnsPath(n.ID))
	if err != nil {
		return err
	}
	defer ns.Close()
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: CloneVeth(n.ID)}, PeerName: cloneVethPeer, PeerNamespace: netlink.NsFd(ns),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("creating veth %s: %w", veth.Name, err)
	}
	return inNetns(ns, func() error { return setupInside(peerAddr, hostAddr.Addr(), uid, gid) })
}

// setupInside configures the netns the calling thread is in: links, addresses, route, NAT.
func setupInside(peerAddr netip.Prefix, hostAddr netip.Addr, uid, gid int) error {
	lo, err := netlink.LinkByName("lo")
	if err == nil {
		err = netlink.LinkSetUp(lo)
	}
	if err != nil {
		return fmt.Errorf("bringing up lo: %w", err)
	}
	peer, err := netlink.LinkByName(cloneVethPeer)
	if err != nil {
		return fmt.Errorf("finding %s: %w", cloneVethPeer, err)
	}
	if err := addrUp(peer, peerAddr); err != nil {
		return err
	}
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: CloneTap}, Mode: netlink.TUNTAP_MODE_TAP,
		Owner: uint32(uid), Group: uint32(gid),
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("creating %s: %w", CloneTap, err)
	}
	mac, _ := net.ParseMAC(CloneTapMAC)
	if err := netlink.LinkSetHardwareAddr(tap, mac); err != nil {
		return fmt.Errorf("setting %s's MAC: %w", CloneTap, err)
	}
	if err := addrUp(tap, netip.MustParsePrefix(CloneTapAddr)); err != nil {
		return err
	}
	route := &netlink.Route{LinkIndex: peer.Attrs().Index, Gw: net.IP(hostAddr.AsSlice())}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("adding the default route via %s: %w", hostAddr, err)
	}
	// /proc/sys/net belongs to the netns of the thread that opens it.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("turning on forwarding: %w", err)
	}
	// A child process starts in the netns of the thread that forks it, this locked one.
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(cloneNftRules(hostAddr))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("loading the clone's nftables rules: %w: %s", err, out)
	}
	return nil
}

func addrUp(link netlink.Link, addr netip.Prefix) error {
	nlAddr, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, nlAddr); err != nil {
		return fmt.Errorf("setting %s on %s: %w", addr, link.Attrs().Name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up %s: %w", link.Attrs().Name, err)
	}
	return nil
}

// cloneNftRules is the clone netns's table: the guest's DNS goes to anvild on the host
// end, its traffic leaves masqueraded as the netns end, and nothing comes in unasked.
func cloneNftRules(hostAddr netip.Addr) string {
	gw := netip.MustParsePrefix(CloneTapAddr).Addr()
	return fmt.Sprintf(`table ip anvil-clone {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "%[1]s" ip daddr %[3]s udp dport 53 dnat to %[4]s
		iifname "%[1]s" ip daddr %[3]s tcp dport 53 dnat to %[4]s
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "%[2]s" masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
		iifname "%[1]s" oifname "%[2]s" accept
		iifname "%[2]s" oifname "%[1]s" ct state established,related accept
	}
	chain input {
		type filter hook input priority filter; policy drop;
		iifname "lo" accept
		ct state established,related accept
		iifname "%[1]s" icmp type echo-request accept
	}
}
`, CloneTap, cloneVethPeer, gw, hostAddr)
}

// pinNewNetns creates a network namespace and bind-mounts it at path, as `ip netns add` does.
func pinNewNetns(path string) (netns.NsHandle, error) {
	if err := os.MkdirAll(NetnsDir, 0o755); err != nil {
		return netns.None(), err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return netns.None(), fmt.Errorf("creating %s: %w", path, err)
	}
	f.Close()

	err = onNewNetnsThread(func() error {
		src := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		if err := unix.Mount(src, path, "none", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("pinning the netns at %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return netns.None(), err
	}
	return netns.GetFromPath(path)
}

// onNewNetnsThread runs fn on a thread moved to a new network namespace.
func onNewNetnsThread(fn func() error) error {
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	defer orig.Close()
	created, err := netns.New()
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("creating a netns: %w", err)
	}
	created.Close()
	return backTo(orig, fn())
}

// inNetns runs fn on a thread in ns.
func inNetns(ns netns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	defer orig.Close()
	if err := netns.Set(ns); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("entering the clone netns: %w", err)
	}
	return backTo(orig, fn())
}

// backTo moves the locked thread back to orig. A thread that can't go back stays locked,
// so Go ends it with its goroutine instead of reusing it.
func backTo(orig netns.NsHandle, fnErr error) error {
	if err := netns.Set(orig); err != nil {
		return errors.Join(fnErr, fmt.Errorf("returning to the host netns: %w", err))
	}
	runtime.UnlockOSThread()
	return fnErr
}

// deleteNetns removes clone id's veth pair and unpins its netns. Devices inside go with
// the netns once nothing (a firecracker) still runs in it.
func deleteNetns(id string) error {
	if !cloneIDRe.MatchString(id) {
		return fmt.Errorf("invalid clone id %q", id)
	}
	if l, err := netlink.LinkByName(CloneVeth(id)); err == nil {
		if err := netlink.LinkDel(l); err != nil {
			return fmt.Errorf("deleting veth %s: %w", l.Attrs().Name, err)
		}
	}
	path := NetnsPath(id)
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unpinning %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
