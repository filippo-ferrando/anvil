//go:build linux

// Package relay forwards a unix socket to another one, passing along any file
// descriptors (SCM_RIGHTS) sent with the data, as Wayland and X11 clients need.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// maxFDs is how many descriptors one read can carry; Wayland sends at most 28 per message.
const maxFDs = 64

// Relay accepts clients on one socket and connects each of them to an upstream socket.
type Relay struct {
	ln       *net.UnixListener
	upstream string

	mu       sync.Mutex
	conns    map[*net.UnixConn]struct{}
	active   int
	used     bool
	lastBusy time.Time
	closed   bool
	wg       sync.WaitGroup
}

// Listen creates listenPath, world-connectable (the directory around it limits access).
// An existing file there is replaced.
func Listen(listenPath, upstreamPath string) (*Relay, error) {
	if err := os.Remove(listenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("relay: removing stale %s: %w", listenPath, err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: listenPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("relay: listening on %s: %w", listenPath, err)
	}
	ln.SetUnlinkOnClose(true)
	if err := os.Chmod(listenPath, 0o777); err != nil {
		ln.Close()
		return nil, fmt.Errorf("relay: chmod %s: %w", listenPath, err)
	}
	return &Relay{ln: ln, upstream: upstreamPath, conns: make(map[*net.UnixConn]struct{}), lastBusy: time.Now()}, nil
}

// Serve accepts clients until Close, then waits for their connections to end.
func (r *Relay) Serve() error {
	for {
		client, err := r.ln.AcceptUnix()
		if err != nil {
			r.wg.Wait()
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("relay: accepting: %w", err)
		}
		upstream, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.upstream, Net: "unix"})
		if err != nil {
			client.Close()
			continue // the app sees a closed connection; keep serving others
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			client.Close()
			upstream.Close()
			continue
		}
		r.conns[client], r.conns[upstream] = struct{}{}, struct{}{}
		r.active++
		r.used = true
		r.mu.Unlock()

		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			Pipe(client, upstream)
			r.mu.Lock()
			delete(r.conns, client)
			delete(r.conns, upstream)
			r.active--
			r.lastBusy = time.Now()
			r.mu.Unlock()
		}()
	}
}

// Close stops accepting, removes the socket and cuts every open connection.
func (r *Relay) Close() {
	r.mu.Lock()
	r.closed = true
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
	r.ln.Close()
}

// Used reports whether any client connected so far.
func (r *Relay) Used() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.used
}

// idleFor reports how long the relay has had no open connection, 0 while one is open.
func (r *Relay) idleFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active > 0 {
		return 0
	}
	return time.Since(r.lastBusy)
}

// WaitIdle returns once none of relays has had an open connection for grace, or ctx ends.
func WaitIdle(ctx context.Context, grace time.Duration, relays ...*Relay) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		idle := true
		for _, r := range relays {
			if r.idleFor() < grace {
				idle = false
				break
			}
		}
		if idle {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Serve runs a relay from listenPath to upstreamPath until ctx ends.
func Serve(ctx context.Context, listenPath, upstreamPath string) error {
	r, err := Listen(listenPath, upstreamPath)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, r.Close)
	defer stop()
	return r.Serve()
}

// Pipe copies a and b into each other, fds included, until both sides are done, then closes both.
func Pipe(a, b *net.UnixConn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyMsgs(b, a) }()
	go func() { defer wg.Done(); copyMsgs(a, b) }()
	wg.Wait()
	a.Close()
	b.Close()
}

// copyMsgs moves data and fds from src to dst. On EOF it half-closes dst; on error it closes both.
func copyMsgs(dst, src *net.UnixConn) {
	buf := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(maxFDs*4))
	for {
		n, oobn, flags, _, err := src.ReadMsgUnix(buf, oob)
		fds, perr := parseFDs(oob[:oobn])
		if perr == nil && flags&syscall.MSG_CTRUNC != 0 {
			perr = errors.New("control data truncated")
		}
		if perr != nil {
			closeFDs(fds)
			src.Close()
			dst.Close()
			return
		}
		if n > 0 || len(fds) > 0 {
			werr := writeMsg(dst, buf[:n], fds)
			closeFDs(fds)
			if werr != nil {
				src.Close()
				dst.Close()
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = dst.CloseWrite()
				return
			}
			src.Close()
			dst.Close()
			return
		}
		if n == 0 {
			_ = dst.CloseWrite()
			return
		}
	}
}

// writeMsg sends the fds with the first chunk of data, then the rest of the data as plain writes.
func writeMsg(dst *net.UnixConn, data []byte, fds []int) error {
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	if len(data) == 0 {
		// A stream socket always delivers fds with at least one byte of data.
		return errors.New("relay: fds received without data")
	}
	n, _, err := dst.WriteMsgUnix(data, oob, nil)
	for err == nil && n < len(data) {
		var m int
		m, err = dst.Write(data[n:])
		n += m
	}
	return err
}

func parseFDs(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_SOCKET || m.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		got, err := syscall.ParseUnixRights(&m)
		if err != nil {
			closeFDs(fds)
			return nil, err
		}
		fds = append(fds, got...)
	}
	return fds, nil
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}
