//go:build linux

package vm

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
)

// TestCloneNetworkReal sets up a clone network with the helper's code run in process, then
// asks anvild's DNS server a question from inside the clone netns. Run under `unshare -rnm`.
func TestCloneNetworkReal(t *testing.T) {
	data, _ := os.ReadFile("/proc/self/uid_map")
	if os.Geteuid() != 0 || strings.Join(strings.Fields(string(data)), " ") == "0 0 4294967295" {
		t.Skip("needs its own user, network and mount namespaces, run under unshare -rnm")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	hostdev.NetnsDir = t.TempDir()
	t.Cleanup(func() { hostdev.NetnsDir = "/run/netns" })
	// With lo up the unreachable upstream refuses at once, so the server answers SERVFAIL fast.
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}

	ctx := context.Background()
	changes := 0
	c := &CloneNetworks{Helper: hostdev.LocalNetns{UID: os.Getuid(), GID: os.Getgid()}, OnChange: func() { changes++ }}
	cn, err := c.Setup(ctx, "eph-REAL1")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if cn.NetNS != hostdev.NetnsPath("eph-REAL1") || cn.Tap != hostdev.CloneTap || cn.Host.String() != "10.253.0.1" {
		t.Errorf("CloneNet = %+v", cn)
	}
	if out, err := exec.Command("nft", "list", "table", "ip", "anvil-eph").CombinedOutput(); err != nil {
		t.Errorf("host clone table: %v: %s", err, out)
	}
	second, err := c.Setup(ctx, "eph-REAL2")
	if err != nil || second.Host.String() != "10.253.0.5" {
		t.Fatalf("second Setup = %+v, %v", second, err)
	}

	srv := dns.NewServer(func(context.Context) ([]dns.Zone, error) { return c.DNSZones(), nil })
	defer srv.Close()
	if err := srv.Refresh(ctx); err != nil {
		t.Fatalf("DNS server on the clone links: %v", err)
	}
	// Without an upstream the server can't resolve, but any answer means the query got through.
	err = inCloneNetns(t, cn.NetNS, func() error {
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(cn.Host.String(), "53"))
		}}
		qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		_, err := r.LookupHost(qctx, "example.com")
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsTimeout {
			return err
		}
		return nil
	})
	if err != nil {
		t.Errorf("DNS from inside the clone: %v", err)
	}
	// The host end of another clone's link is the host too, but not this clone's to reach.
	err = inCloneNetns(t, cn.NetNS, func() error {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(second.Host.String(), "53"), 2*time.Second)
		if err == nil {
			conn.Close()
			return errors.New("connected")
		}
		return nil
	})
	if err != nil {
		t.Errorf("DNS of clone 2 from clone 1: %v", err)
	}

	if err := c.Teardown(ctx, "eph-REAL1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := netlink.LinkByName(hostdev.CloneVeth("eph-REAL1")); err == nil {
		t.Error("veth still there after Teardown")
	}
	if len(c.DNSZones()) != 1 || changes != 3 {
		t.Errorf("after Teardown: %d zones, %d changes", len(c.DNSZones()), changes)
	}

	// A new anvild finds eph-REAL2 as a leftover.
	fresh := &CloneNetworks{Helper: c.Helper}
	if err := fresh.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(second.NetNS); !os.IsNotExist(err) {
		t.Errorf("Sweep left %s: %v", second.NetNS, err)
	}
}

// inCloneNetns runs fn on a thread in the netns pinned at path.
func inCloneNetns(t *testing.T, path string, fn func() error) error {
	t.Helper()
	ns, err := netns.GetFromPath(path)
	if err != nil {
		return err
	}
	defer ns.Close()
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return err // the thread stays locked and dies with the goroutine
	}
	defer orig.Close()
	if err := netns.Set(ns); err != nil {
		return err
	}
	fnErr := fn()
	if err := netns.Set(orig); err != nil {
		return err
	}
	runtime.UnlockOSThread()
	return fnErr
}
