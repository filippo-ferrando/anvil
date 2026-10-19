//go:build linux

package vm

import (
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// mountTag mounts /tag from the hub listening on 127.0.0.1, as an NFS client would.
func mountTag(t *testing.T, tag string) (*nfsc.Target, error) {
	t.Helper()
	c, err := rpc.DialTCP("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fcNFSPort)), false)
	if err != nil {
		return nil, err
	}
	t.Cleanup(c.Close)
	mounter := &nfsc.Mount{Client: c}
	return mounter.Mount("/"+tag, rpc.AuthNull)
}

func TestNFSHub(t *testing.T) {
	loopback := netip.MustParseAddr("127.0.0.1")
	rw, ro := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "hello.txt"), []byte("from host"), 0o644); err != nil {
		t.Fatal(err)
	}

	hub := newNFSHub()
	// The guest is 127.0.0.1 here, the address this test connects from.
	if err := hub.export(loopback, loopback, "mount0", rw, false); err != nil {
		t.Skipf("can't listen on 127.0.0.1:%d: %v", fcNFSPort, err)
	}
	if err := hub.export(loopback, loopback, "mount1", ro, true); err != nil {
		t.Fatal(err)
	}

	target, err := mountTag(t, "mount0")
	if err != nil {
		t.Fatalf("mounting mount0: %v", err)
	}
	f, err := target.OpenFile("/written.txt", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("from guest")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if data, err := os.ReadFile(filepath.Join(rw, "written.txt")); err != nil || string(data) != "from guest" {
		t.Fatalf("host sees %q, %v", data, err)
	}
	if err := target.Setattr("/written.txt", nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o600}}); err != nil {
		t.Fatalf("chmod through the share: %v", err)
	}
	if info, _ := os.Stat(filepath.Join(rw, "written.txt")); info.Mode().Perm() != 0o600 {
		t.Fatalf("host mode after chmod = %v", info.Mode().Perm())
	}

	roTarget, err := mountTag(t, "mount1")
	if err != nil {
		t.Fatalf("mounting mount1: %v", err)
	}
	rf, err := roTarget.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rf)
	rf.Close()
	if string(data) != "from host" {
		t.Fatalf("read %q from the read-only share", data)
	}
	if _, err := roTarget.Create("/nope.txt", 0o644); err == nil {
		t.Fatal("created a file on a read-only share")
	}
	if _, err := os.Stat(filepath.Join(ro, "nope.txt")); err == nil {
		t.Fatal("the read-only share's host dir got a new file")
	}

	if _, err := mountTag(t, "mount9"); err == nil {
		t.Fatal("mounted a tag that was never exported")
	}

	// Once the VM is forgotten its connections are closed straight away.
	hub.forget(loopback)
	expectClosed(t, "after the VM was forgotten")

	// Shared with another guest only: this address gets nothing.
	if err := hub.export(loopback, netip.MustParseAddr("10.9.9.9"), "mount0", rw, false); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, "from an address that isn't the guest's")
}

// expectClosed checks the hub closes a new connection from 127.0.0.1 without answering.
// (The NFS client would retry a closed connection for minutes.)
func expectClosed(t *testing.T, when string) {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fcNFSPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection %s: read err = %v, want the hub to close it", when, err)
	}
}

func TestNFSFstabLine(t *testing.T) {
	m := instance.Mount{Tag: "mount3", GuestPath: "/srv/my data", ReadOnly: true}
	line := nfsFstabLine("10.254.254.1", m)
	want := `10.254.254.1:/mount3 /srv/my\040data nfs vers=3,proto=tcp,port=20490,mountport=20490,mountproto=tcp,nolock,addr=10.254.254.1,mountaddr=10.254.254.1,_netdev,nofail,ro 0 0`
	if line != want {
		t.Fatalf("line:\n%s\nwant:\n%s", line, want)
	}

	g := newGuestShell(t, "UUID=abc / ext4 defaults 0 1\n10.254.254.1:/mount3 /old nfs defaults 0 0\n"+line+"\n")
	g.run(umountLiveScript(m))
	if got := g.fstabContent(); strings.Contains(got, "mount3") || !strings.Contains(got, "UUID=abc") {
		t.Fatalf("fstab after umount:\n%s", got)
	}
	out, err := mergeMounts("", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	g = newGuestShell(t, "UUID=abc / ext4 defaults 0 1\n"+line+"\n")
	g.run(bootcmdScript(t, out))
	if got := g.fstabContent(); got != "UUID=abc / ext4 defaults 0 1\n" {
		t.Fatalf("a new generation without mounts left:\n%s", got)
	}
}
