//go:build linux

package vm

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// udpIdleTimeout ends a UDP flow through a portProxy after this long without traffic.
const udpIdleTimeout = 2 * time.Minute

// portProxy forwards one host port to a guest address, in place of a SLIRP hostfwd for a
// VM that sits on a tap device. It listens on all interfaces, like hostfwd does.
type portProxy struct {
	HostPort int
	close    func()
}

func (p *portProxy) Close() { p.close() }

// startPortProxy forwards protocol ("tcp"/"udp") traffic on hostPort to target
// ("ip:port"). hostPort 0 picks a free port, read back from HostPort.
func startPortProxy(protocol string, hostPort int, target string) (*portProxy, error) {
	addr := ":" + strconv.Itoa(hostPort)
	switch protocol {
	case "tcp":
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("vm: listening on %d/tcp: %w", hostPort, err)
		}
		go serveTCP(l, target)
		return &portProxy{HostPort: l.Addr().(*net.TCPAddr).Port, close: func() { l.Close() }}, nil
	case "udp":
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("vm: listening on %d/udp: %w", hostPort, err)
		}
		go serveUDP(pc, target)
		return &portProxy{HostPort: pc.LocalAddr().(*net.UDPAddr).Port, close: func() { pc.Close() }}, nil
	default:
		return nil, fmt.Errorf("vm: unknown protocol %q", protocol)
	}
}

func serveTCP(l net.Listener, target string) {
	for {
		client, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("vm: port proxy to %s: %v", target, err)
			}
			return
		}
		go func() {
			defer client.Close()
			guest, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				return // the guest isn't listening (yet): the client sees the connection close
			}
			defer guest.Close()
			done := make(chan struct{}, 2)
			pipe := func(dst, src net.Conn) {
				_, _ = io.Copy(dst, src)
				// Pass the half-close on, so the other side sees EOF.
				if tc, ok := dst.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				done <- struct{}{}
			}
			go pipe(guest, client)
			go pipe(client, guest)
			<-done
			<-done
		}()
	}
}

// serveUDP keeps one guest-side socket per client address, so replies find their way back.
func serveUDP(pc net.PacketConn, target string) {
	var mu sync.Mutex
	flows := map[string]net.Conn{}
	buf := make([]byte, 64<<10)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			mu.Lock()
			for _, c := range flows {
				c.Close()
			}
			mu.Unlock()
			return
		}
		mu.Lock()
		guest, ok := flows[client.String()]
		if !ok {
			guest, err = net.Dial("udp", target)
			if err != nil {
				mu.Unlock()
				continue
			}
			flows[client.String()] = guest
			go func(key string, client net.Addr, guest net.Conn) {
				reply := make([]byte, 64<<10)
				for {
					_ = guest.SetReadDeadline(time.Now().Add(udpIdleTimeout))
					n, err := guest.Read(reply)
					if err != nil {
						break
					}
					_, _ = pc.WriteTo(reply[:n], client)
				}
				mu.Lock()
				delete(flows, key)
				mu.Unlock()
				guest.Close()
			}(client.String(), client, guest)
		}
		mu.Unlock()
		_, _ = guest.Write(buf[:n])
	}
}
