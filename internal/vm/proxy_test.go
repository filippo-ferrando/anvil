//go:build linux

package vm

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestPortProxyTCP(t *testing.T) {
	guest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	go func() {
		for {
			c, err := guest.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()

	p, err := startPortProxy("tcp", 0, guest.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.HostPort)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	// Half-closing our side must reach the guest, whose echo then ends too.
	_ = conn.(*net.TCPConn).CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "ping" {
		t.Fatalf("echo through the proxy = %q, %v", got, err)
	}
	conn.Close()

	p.Close()
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.HostPort)), time.Second); err == nil {
		c.Close()
		t.Fatal("the proxy still accepts connections after Close")
	}
}

func TestPortProxyUDP(t *testing.T) {
	guest, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := guest.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = guest.WriteTo(append([]byte("re:"), buf[:n]...), from)
		}
	}()

	p, err := startPortProxy("udp", 0, guest.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Two clients, so replies must go back to the right one.
	for _, msg := range []string{"one", "two"} {
		conn, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.HostPort)))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte(msg))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		conn.Close()
		if err != nil || string(buf[:n]) != "re:"+msg {
			t.Fatalf("reply to %q = %q, %v", msg, buf[:n], err)
		}
	}
}
