//go:build linux

package hostdev

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeConn(t *testing.T) {
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	served := make(chan error, 1)
	go func() {
		conn, err := l.AcceptUnix()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		served <- ServeConn(conn)
	}()
	// A request without its netns is refused by handle, so nothing runs as root.
	_, err = Client{Socket: sock}.call(context.Background(), request{Op: "netns-create"})
	if err == nil || !strings.Contains(err.Error(), "without a netns") {
		t.Errorf("call error = %v, want the helper's refusal", err)
	}
	if err := <-served; err != nil {
		t.Errorf("ServeConn: %v", err)
	}
}
