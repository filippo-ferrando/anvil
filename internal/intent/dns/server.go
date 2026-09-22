// Package dns is a small DNS server that answers for intent members by
// name and forwards every other query to the host's own resolvers.
package dns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// TLD is the top-level domain every intent zone lives under, so a member
// is reachable as "<role>.<intent>.anvil".
const TLD = "anvil"

// DefaultPort is the port the server listens on at each zone's address.
const DefaultPort = 53

// recordTTL is short on purpose, so a member added or removed later is
// picked up by its peers within seconds.
const recordTTL = 5

const forwardTimeout = 3 * time.Second

// Zone is one intent's set of names, served on its network's gateway.
type Zone struct {
	Domain  string                // e.g. "myapp.anvil"
	Listen  netip.Addr            // address to bind on, the intent's gateway
	Subnet  netip.Prefix          // only clients from here (or loopback) are answered
	Records map[string]netip.Addr // label -> address, e.g. "db" -> 10.55.201.4
}

// ZoneSource returns every zone that should currently be served.
type ZoneSource func(ctx context.Context) ([]Zone, error)

// Server runs one UDP and one TCP listener per zone and keeps them in
// sync with its ZoneSource on every Refresh.
type Server struct {
	Source ZoneSource
	Port   int

	// ResolvConf is where upstream resolvers are read from on Refresh.
	ResolvConf string

	mu        sync.Mutex
	listeners map[netip.Addr]*zoneListener
	upstreams []string
}

func NewServer(source ZoneSource) *Server {
	return &Server{
		Source:     source,
		Port:       DefaultPort,
		ResolvConf: "/etc/resolv.conf",
		listeners:  map[netip.Addr]*zoneListener{},
	}
}

// Run refreshes once now and then every interval, until ctx is done.
func (s *Server) Run(ctx context.Context, interval time.Duration) {
	defer s.Close()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Refresh(ctx); err != nil {
			log.Printf("dns: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh re-reads zones and upstreams, opens listeners for new zones and
// closes the ones no longer present. A failed bind is retried on the next call.
func (s *Server) Refresh(ctx context.Context) error {
	zones, err := s.Source(ctx)
	if err != nil {
		return fmt.Errorf("listing zones: %w", err)
	}
	upstreams := readUpstreams(s.ResolvConf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreams = upstreams

	wanted := make(map[netip.Addr]bool, len(zones))
	var errs []error
	for _, z := range zones {
		if !z.Listen.IsValid() {
			continue
		}
		wanted[z.Listen] = true
		if l, ok := s.listeners[z.Listen]; ok {
			l.setZone(z)
			continue
		}
		l, err := s.listen(z)
		if err != nil {
			errs = append(errs, fmt.Errorf("serving %s on %s: %w", z.Domain, z.Listen, err))
			continue
		}
		s.listeners[z.Listen] = l
	}
	for addr, l := range s.listeners {
		if !wanted[addr] {
			l.close()
			delete(s.listeners, addr)
		}
	}
	return errors.Join(errs...)
}

// Close stops every listener.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for addr, l := range s.listeners {
		l.close()
		delete(s.listeners, addr)
	}
}

func (s *Server) currentUpstreams() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upstreams
}

// zoneListener serves one zone. zone is swapped whole on Refresh, never
// mutated, so handlers can read a snapshot without holding mu.
type zoneListener struct {
	mu   sync.RWMutex
	zone Zone

	udp *net.UDPConn
	tcp *net.TCPListener
}

func (l *zoneListener) setZone(z Zone) {
	l.mu.Lock()
	l.zone = z
	l.mu.Unlock()
}

func (l *zoneListener) snapshot() Zone {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.zone
}

func (l *zoneListener) close() {
	l.udp.Close()
	l.tcp.Close()
}

func (s *Server) listen(z Zone) (*zoneListener, error) {
	addr := netip.AddrPortFrom(z.Listen, uint16(s.Port))
	udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return nil, err
	}
	tcp, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addr))
	if err != nil {
		udp.Close()
		return nil, err
	}
	l := &zoneListener{zone: z, udp: udp, tcp: tcp}
	go s.serveUDP(l)
	go s.serveTCP(l)
	return l, nil
}

func allowed(z Zone, from netip.Addr) bool {
	from = from.Unmap()
	return from.IsLoopback() || (z.Subnet.IsValid() && z.Subnet.Contains(from))
}

func (s *Server) serveUDP(l *zoneListener) {
	buf := make([]byte, 65535)
	for {
		n, from, err := l.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return // closed
		}
		z := l.snapshot()
		if !allowed(z, from.Addr()) {
			continue
		}
		req := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := s.handle(z, req, "udp"); resp != nil {
				_, _ = l.udp.WriteToUDPAddrPort(resp, from)
			}
		}()
	}
}

func (s *Server) serveTCP(l *zoneListener) {
	for {
		conn, err := l.tcp.AcceptTCP()
		if err != nil {
			return // closed
		}
		from := conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr()
		if !allowed(l.snapshot(), from) {
			conn.Close()
			continue
		}
		go func() {
			defer conn.Close()
			for {
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				req, err := readTCPMessage(conn)
				if err != nil {
					return
				}
				resp := s.handle(l.snapshot(), req, "tcp")
				if resp == nil || writeTCPMessage(conn, resp) != nil {
					return
				}
			}
		}()
	}
}

// handle answers req from z when the name is inside z.Domain, otherwise
// forwards it upstream. A nil reply means drop the query.
func (s *Server) handle(z Zone, req []byte, network string) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(req)
	if err != nil || hdr.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return reply(hdr, nil, dnsmessage.RCodeFormatError, nil)
	}

	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	domain := strings.ToLower(z.Domain)
	if name != domain && !strings.HasSuffix(name, "."+domain) {
		return s.forward(hdr, q, req, network)
	}

	if name == domain {
		return reply(hdr, &q, dnsmessage.RCodeSuccess, nil)
	}
	label := strings.TrimSuffix(name, "."+domain)
	addr, ok := z.Records[label]
	if !ok || strings.Contains(label, ".") {
		return reply(hdr, &q, dnsmessage.RCodeNameError, nil)
	}
	if q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeALL {
		if addr.Is4() {
			return reply(hdr, &q, dnsmessage.RCodeSuccess, &addr)
		}
	}
	// The name exists but has no record of this type.
	return reply(hdr, &q, dnsmessage.RCodeSuccess, nil)
}

// reply builds an authoritative answer to hdr/q with at most one A record.
func reply(hdr dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, a *netip.Addr) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		OpCode:             hdr.OpCode,
		Authoritative:      q != nil,
		RecursionDesired:   hdr.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	})
	b.EnableCompression()
	if q != nil {
		if err := b.StartQuestions(); err != nil {
			return nil
		}
		if err := b.Question(*q); err != nil {
			return nil
		}
	}
	if q != nil && a != nil {
		if err := b.StartAnswers(); err != nil {
			return nil
		}
		rh := dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: recordTTL}
		if err := b.AResource(rh, dnsmessage.AResource{A: a.As4()}); err != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

// forward relays req unchanged to the first upstream that answers.
func (s *Server) forward(hdr dnsmessage.Header, q dnsmessage.Question, req []byte, network string) []byte {
	for _, up := range s.currentUpstreams() {
		resp, err := exchange(network, up, req)
		if err == nil {
			return resp
		}
	}
	return reply(hdr, &q, dnsmessage.RCodeServerFailure, nil)
}

func exchange(network, addr string, req []byte) ([]byte, error) {
	conn, err := net.DialTimeout(network, addr, forwardTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(forwardTimeout))
	if network == "tcp" {
		if err := writeTCPMessage(conn, req); err != nil {
			return nil, err
		}
		return readTCPMessage(conn)
	}
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// readTCPMessage reads one DNS message with its 2-byte length prefix.
func readTCPMessage(conn net.Conn) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, int(size[0])<<8|int(size[1]))
	if _, err := io.ReadFull(conn, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func writeTCPMessage(conn net.Conn, msg []byte) error {
	if len(msg) > 0xffff {
		return fmt.Errorf("message too large")
	}
	_, err := conn.Write(append([]byte{byte(len(msg) >> 8), byte(len(msg))}, msg...))
	return err
}

// Label turns s into a valid DNS label: lowercase letters, digits and
// dashes only, at most 63 characters.
func Label(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}

// Domain returns the zone name for an intent, e.g. "myapp.anvil".
func Domain(intentName string) string {
	return Label(intentName) + "." + TLD
}

// readUpstreams returns every "nameserver" in path as a host:port pair.
func readUpstreams(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil {
			continue
		}
		out = append(out, net.JoinHostPort(addr.String(), strconv.Itoa(53)))
	}
	return out
}
