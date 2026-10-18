//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// vsockConn is a host-initiated vsock connection. Reads go through the reader that
// consumed the handshake reply, so nothing the guest sent right after it is lost.
type vsockConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *vsockConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// DialVsock connects to port inside the guest through Firecracker's vsock unix socket,
// using its "CONNECT <port>" handshake.
func DialVsock(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("firecracker: dialing vsock socket %s: %w", udsPath, err)
	}
	// The handshake gets a few seconds; a guest without a listener closes the connection.
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("firecracker: vsock handshake: %w", err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("firecracker: nothing listening on guest vsock port %d: %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("firecracker: vsock handshake to port %d refused: %q", port, strings.TrimSpace(line))
	}
	_ = conn.SetDeadline(time.Time{})
	return &vsockConn{Conn: conn, r: r}, nil
}
