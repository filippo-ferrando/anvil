package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/peer"
)

func TestCallerListenerReportsPeerUID(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := net.Dial("unix", sock); err == nil {
			defer c.Close()
			_, _ = c.Read(make([]byte, 1))
		}
	}()
	c, err := CallerListener(l).Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: c.RemoteAddr()})
	if uid, ok := callerUID(ctx); !ok || int(uid) != os.Getuid() {
		t.Errorf("callerUID = %d, %v; want %d", uid, ok, os.Getuid())
	}
}

func TestCheckCallerOwns(t *testing.T) {
	dir := t.TempDir()
	asCaller := func(uid uint32) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: callerAddr{uid: uid}})
	}

	p := dir
	if err := checkCallerOwns(context.Background(), &p); err == nil {
		t.Error("expected an unidentified caller to be refused")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root, every caller below would be the daemon's own user")
	}
	if err := checkCallerOwns(asCaller(uint32(os.Getuid())), &p); err != nil {
		t.Errorf("the daemon's own user should pass: %v", err)
	}
	// A third user owns neither the temp dir nor "/".
	for _, path := range []string{dir, "/"} {
		p := path
		if err := checkCallerOwns(asCaller(54321), &p); err == nil {
			t.Errorf("expected uid 54321 to be refused %s", path)
		}
	}
	if err := checkCallerOwns(asCaller(54321)); err != nil {
		t.Errorf("no paths means nothing to check: %v", err)
	}
}

func TestCheckCallerFile(t *testing.T) {
	dir := t.TempDir()
	me := peer.NewContext(context.Background(), &peer.Peer{Addr: callerAddr{uid: uint32(os.Getuid())}})
	file := filepath.Join(dir, "bundle.tar.zst")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "new.tar.zst")
	if err := checkCallerFile(me, &p, false); err != nil {
		t.Errorf("a new file in an owned dir should pass: %v", err)
	}
	if err := checkCallerFile(me, &p, true); err == nil {
		t.Error("expected a missing file to fail when it must exist")
	}
	p = file
	if err := checkCallerFile(me, &p, true); err != nil {
		t.Errorf("an owned regular file should pass: %v", err)
	}
	p = link
	if err := checkCallerFile(me, &p, false); err == nil {
		t.Error("expected a symlink to be refused")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root, every caller is the daemon's own user")
	}
	other := peer.NewContext(context.Background(), &peer.Peer{Addr: callerAddr{uid: 54321}})
	p = file
	if err := checkCallerFile(other, &p, true); err == nil {
		t.Error("expected uid 54321 to be refused a file it doesn't own")
	}
}
