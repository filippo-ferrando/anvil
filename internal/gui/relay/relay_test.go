//go:build linux

package relay

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// startRelay runs Serve between a fresh listen socket and upstream, returning the listen path.
func startRelay(t *testing.T, upstream string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listen := filepath.Join(t.TempDir(), "relay.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listen, upstream) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(listen); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return listen, cancel, done
}

func dial(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func inode(t *testing.T, fd int) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

// recvWithFD reads a message and returns its data and the single fd that came with it.
func recvWithFD(t *testing.T, c *net.UnixConn) ([]byte, int) {
	t.Helper()
	buf := make([]byte, 1024)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatal(err)
	}
	fds, err := parseFDs(oob[:oobn])
	if err != nil || len(fds) != 1 {
		t.Fatalf("want 1 fd, got %v (err %v)", fds, err)
	}
	return buf[:n], fds[0]
}

func TestServe_PassesDataAndFDsBothWays(t *testing.T) {
	upstreamPath := filepath.Join(t.TempDir(), "upstream.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: upstreamPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	listen, cancel, done := startRelay(t, upstreamPath)
	defer cancel()

	client := dial(t, listen)
	defer client.Close()
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	f, err := os.CreateTemp(t.TempDir(), "shm")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := inode(t, int(f.Fd()))

	// client -> server, with an fd
	if _, _, err := client.WriteMsgUnix([]byte("hello"), syscall.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	data, fd := recvWithFD(t, server)
	defer syscall.Close(fd)
	if string(data) != "hello" {
		t.Fatalf("server got %q", data)
	}
	if got := inode(t, fd); got != want {
		t.Fatalf("fd points to inode %d, want %d", got, want)
	}

	// server -> client, with an fd
	if _, _, err := server.WriteMsgUnix([]byte("back"), syscall.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	data, fd2 := recvWithFD(t, client)
	defer syscall.Close(fd2)
	if string(data) != "back" || inode(t, fd2) != want {
		t.Fatalf("client got %q / inode %d", data, inode(t, fd2))
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := os.Stat(listen); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay socket still there after stop: %v", err)
	}
}

func TestServe_LargeTransferAndHalfClose(t *testing.T) {
	upstreamPath := filepath.Join(t.TempDir(), "upstream.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: upstreamPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	listen, cancel, _ := startRelay(t, upstreamPath)
	defer cancel()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	client := dial(t, listen)
	defer client.Close()
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	go func() {
		_, _ = client.Write(payload)
		_ = client.CloseWrite()
	}()
	var got bytes.Buffer
	if _, err := got.ReadFrom(server); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("got %d bytes, want %d", got.Len(), len(payload))
	}

	// The other direction still works after the client's half-close.
	if _, err := server.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := client.Read(buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read after half-close: %q %v", buf, err)
	}
}

func TestServe_SocketIsWorldConnectable(t *testing.T) {
	listen, cancel, _ := startRelay(t, filepath.Join(t.TempDir(), "nothing.sock"))
	defer cancel()
	st, err := os.Stat(listen)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o777 {
		t.Fatalf("mode %v, want 0777", st.Mode().Perm())
	}
}

func TestServe_UpstreamMissingClosesClientOnly(t *testing.T) {
	listen, cancel, done := startRelay(t, filepath.Join(t.TempDir(), "nothing.sock"))
	defer cancel()
	c := dial(t, listen)
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the client to be closed")
	}
	select {
	case err := <-done:
		t.Fatalf("Serve stopped: %v", err)
	default:
	}
}

func TestClaim_HandsOutFreeNumbers(t *testing.T) {
	dir := t.TempDir()
	a, releaseA, err := Claim(dir, "X")
	if err != nil || a != 0 {
		t.Fatalf("first claim: %d %v", a, err)
	}
	b, releaseB, err := Claim(dir, "X")
	if err != nil || b != 1 {
		t.Fatalf("second claim: %d %v", b, err)
	}
	releaseA()
	c, releaseC, err := Claim(dir, "X")
	if err != nil || c != 0 {
		t.Fatalf("claim after release: %d %v", c, err)
	}
	releaseB()
	releaseC()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("lock files left behind: %v", entries)
	}
}

func TestWaitIdle_WaitsForOpenConnections(t *testing.T) {
	upstreamPath := filepath.Join(t.TempDir(), "upstream.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: upstreamPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r, err := Listen(filepath.Join(t.TempDir(), "relay.sock"), upstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = r.Serve() }()
	defer r.Close()

	c := dial(t, r.ln.Addr().String())
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	done := make(chan struct{})
	go func() { WaitIdle(context.Background(), 200*time.Millisecond, r); close(done) }()
	select {
	case <-done:
		t.Fatal("WaitIdle returned while a connection was open")
	case <-time.After(500 * time.Millisecond):
	}
	c.Close()
	_, _ = server.Read(make([]byte, 1)) // EOF, as a compositor sees it
	server.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WaitIdle didn't return after the connection closed")
	}
}
