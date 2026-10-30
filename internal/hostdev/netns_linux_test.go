//go:build linux

package hostdev

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// inUserNamespace reports whether this process runs in a user namespace (unshare -r),
// where root can't touch the host's network.
func inUserNamespace() bool {
	data, err := os.ReadFile("/proc/self/uid_map")
	return err == nil && strings.Join(strings.Fields(string(data)), " ") != "0 0 4294967295"
}

// requireCloneNetns skips t unless it can build clone netns safely: as root of its own user,
// network and mount namespaces. Run the test binary under `unshare -rnm`.
func requireCloneNetns(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || !inUserNamespace() {
		t.Skip("needs its own user, network and mount namespaces, run under unshare -rnm")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	NetnsDir = t.TempDir()
	t.Cleanup(func() { NetnsDir = "/run/netns" })
}

func TestCreateNetnsReal(t *testing.T) {
	requireCloneNetns(t)
	n := Netns{ID: "eph-TEST1", Link: "10.253.0.8/30"}
	if err := createNetns(n, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("createNetns: %v", err)
	}
	t.Cleanup(func() { _ = deleteNetns(n.ID) })

	hostEnd, err := netlink.LinkByName(CloneVeth(n.ID))
	if err != nil {
		t.Fatalf("host end of the veth pair: %v", err)
	}
	// anvild's part: the host end's address.
	hostAddr, _, _ := CloneLink(n.Link)
	if err := addrUp(hostEnd, hostAddr); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort(hostAddr.Addr().String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()

	ns, err := netns.GetFromPath(NetnsPath(n.ID))
	if err != nil {
		t.Fatalf("netns not pinned at %s: %v", NetnsPath(n.ID), err)
	}
	defer ns.Close()
	err = inNetns(ns, func() error {
		tap, err := netlink.LinkByName(CloneTap)
		if err != nil {
			t.Errorf("no %s in the netns: %v", CloneTap, err)
		} else if tt, ok := tap.(*netlink.Tuntap); !ok || tt.Mode != netlink.TUNTAP_MODE_TAP || tt.Owner != uint32(os.Getuid()) {
			t.Errorf("%s = %+v, want a tap owned by uid %d", CloneTap, tap, os.Getuid())
		} else if tap.Attrs().HardwareAddr.String() != CloneTapMAC {
			t.Errorf("%s has MAC %s, want the fixed %s", CloneTap, tap.Attrs().HardwareAddr, CloneTapMAC)
		}
		for name, want := range map[string]string{CloneTap: CloneTapAddr, cloneVethPeer: "10.253.0.10/30"} {
			link, err := netlink.LinkByName(name)
			if err != nil {
				t.Errorf("no %s in the netns: %v", name, err)
				continue
			}
			addrs, _ := netlink.AddrList(link, netlink.FAMILY_V4)
			if len(addrs) != 1 || addrs[0].IPNet.String() != want {
				t.Errorf("%s addresses = %v, want %s", name, addrs, want)
			}
		}
		if data, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); strings.TrimSpace(string(data)) != "1" {
			t.Error("forwarding is off in the netns")
		}
		if out, err := exec.Command("nft", "list", "table", "ip", "anvil-clone").CombinedOutput(); err != nil ||
			!strings.Contains(string(out), "dnat to "+hostAddr.Addr().String()) {
			t.Errorf("nft table in the netns: %v: %s", err, out)
		}
		// The connected route reaches the host end, the default route goes through it.
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Errorf("dialing the host end from the netns: %v", err)
		} else {
			c.Close()
		}
		routes, _ := netlink.RouteGet(net.ParseIP("1.1.1.1"))
		if len(routes) != 1 || !routes[0].Gw.Equal(net.IP(hostAddr.Addr().AsSlice())) {
			t.Errorf("route to the outside = %+v, want via %s", routes, hostAddr.Addr())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(CloneTap); err == nil {
		t.Errorf("%s leaked into the test's own netns", CloneTap)
	}

	// Creating it again replaces the leftover.
	if err := createNetns(n, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("createNetns over a leftover: %v", err)
	}
	for range 2 {
		if err := deleteNetns(n.ID); err != nil {
			t.Fatalf("deleteNetns: %v", err)
		}
	}
	if _, err := os.Stat(NetnsPath(n.ID)); !os.IsNotExist(err) {
		t.Errorf("netns file still there: %v", err)
	}
	if _, err := netlink.LinkByName(CloneVeth(n.ID)); err == nil {
		t.Error("veth still there after deleteNetns")
	}
}

func TestCreateNetnsFailureCleansUp(t *testing.T) {
	requireCloneNetns(t)
	n := Netns{ID: "eph-TEST2", Link: "10.253.0.12/30"}
	// Without nft the create fails at its last step, after the netns and veth exist.
	t.Setenv("PATH", t.TempDir())
	if err := createNetns(n, os.Getuid(), os.Getgid()); err == nil || !strings.Contains(err.Error(), "nftables") {
		t.Fatalf("createNetns without nft = %v", err)
	}
	if _, err := os.Stat(NetnsPath(n.ID)); !os.IsNotExist(err) {
		t.Errorf("a failed create left its netns: %v", err)
	}
	if _, err := netlink.LinkByName(CloneVeth(n.ID)); err == nil {
		t.Error("a failed create left its veth")
	}
}
