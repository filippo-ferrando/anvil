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
