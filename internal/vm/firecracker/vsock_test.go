//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
)

// fakeVsock answers one connection on a unix socket like firecracker's vsock device:
// reply is sent for the CONNECT line, then greeting, then whatever comes back is echoed.
func fakeVsock(t *testing.T, reply, greeting string) (string, chan string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "v.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
		if reply == "" {
			return // nothing listening in the guest: firecracker just closes
		}
		_, _ = io.WriteString(conn, reply+greeting)
		_, _ = io.Copy(conn, conn)
	}()
	return sock, got
}

func TestDialVsock(t *testing.T) {
	// The greeting arrives in the same write as the OK line and must not be lost.
	sock, got := fakeVsock(t, "OK 1073741824\n", "hello")
	conn, err := DialVsock(context.Background(), sock, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if line := <-got; line != "CONNECT 1024\n" {
		t.Fatalf("handshake sent %q", line)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read %q, %v; want the greeting", buf, err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q, %v", buf, err)
	}
}

func TestDialVsockNoListener(t *testing.T) {
	sock, _ := fakeVsock(t, "", "")
	if _, err := DialVsock(context.Background(), sock, 1024); err == nil {
		t.Fatal("expected an error when the guest has no listener")
	}
}
