// Package discovery announces this anvil host on the local network over mDNS
// and finds the other ones, so migration targets don't have to be typed by hand.
package discovery

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"

	"github.com/anvil-project/anvil/internal/intent/dns"
)

// Service is the DNS-SD service type anvild announces itself under, so
// `avahi-browse _anvil._tcp` sees the same thing anvil does.
const Service = "_anvil._tcp.local."

const (
	mdnsPort   = 5353
	recordTTL  = 120
	defaultSSH = 22
)

var mdnsGroup = net.IPv4(224, 0, 0, 251)

// Host is one anvild found on the network.
type Host struct {
	Name string     // the remote host's own hostname
	Addr netip.Addr // address it answered from
	Port int        // SSH port from its SRV record
}

// Serve answers mDNS queries for Service with this host's name until ctx is
// done. name and port default to the system hostname and SSH's port 22.
func Serve(ctx context.Context, name string, port int) error {
	if port == 0 {
		port = defaultSSH
	}
	label := hostLabel(name)

	conn, err := listenMulticast()
	if err != nil {
		return err
	}
	defer conn.Close()
	p := ipv4.NewPacketConn(conn)
	// The interface a query arrived on decides which address to answer with.
	if err := p.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return fmt.Errorf("discovery: control messages: %w", err)
	}
	joinAll(p)

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 1500)
	for {
		n, cm, src, err := p.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("discovery: reading mDNS: %w", err)
		}
		if !wantsService(buf[:n]) {
			continue
		}
		ifIndex := 0
		if cm != nil {
			ifIndex = cm.IfIndex
		}
		resp, err := buildResponse(label, port, interfaceAddr(ifIndex))
		if err != nil {
			continue
		}
		// ponytail: always answers unicast to the asker instead of re-multicasting,
		// which skips RFC 6762's shared-record rules but reaches every real client.
		_, _ = p.WriteTo(resp, nil, src)
	}
}

// Browse sends one query and collects the answers that arrive within timeout.
// The local host answers its own query, so its own name is left out.
func Browse(ctx context.Context, timeout time.Duration) ([]Host, error) {
	query, err := buildQuery()
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("discovery: opening query socket: %w", err)
	}
	defer conn.Close()

	p := ipv4.NewPacketConn(conn)
	_ = p.SetMulticastTTL(255)
	dst := &net.UDPAddr{IP: mdnsGroup, Port: mdnsPort}
	sent := 0
	for _, ifi := range multicastInterfaces() {
		if _, err := p.WriteTo(query, &ipv4.ControlMessage{IfIndex: ifi.Index}, dst); err == nil {
			sent++
		}
	}
	if sent == 0 {
		return nil, fmt.Errorf("discovery: no multicast-capable interface to query on")
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	go func() {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()

	self := hostLabel("")
	seen := map[string]Host{}
	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline or cancelled: whatever answered by now is the result
		}
		from, _ := netip.AddrFromSlice(src.IP.To4())
		for _, h := range parseResponse(buf[:n], from) {
			if h.Name == self {
				continue
			}
			if _, dup := seen[h.Name]; !dup {
				seen[h.Name] = h
			}
		}
	}
	out := make([]Host, 0, len(seen))
	for _, h := range seen {
		out = append(out, h)
	}
	return out, nil
}

func buildQuery() ([]byte, error) {
	name, err := dnsmessage.NewName(Service)
	if err != nil {
		return nil, err
	}
	msg := dnsmessage.Message{
		Questions: []dnsmessage.Question{{
			Name: name,
			Type: dnsmessage.TypePTR,
			// Top bit of the class is mDNS's "answer me directly" bit.
			Class: dnsmessage.Class(0x8000 | uint16(dnsmessage.ClassINET)),
		}},
	}
	return msg.Pack()
}

// wantsService reports whether msg is a query for Service.
func wantsService(msg []byte) bool {
	var m dnsmessage.Message
	if err := m.Unpack(msg); err != nil || m.Header.Response {
		return false
	}
	for _, q := range m.Questions {
		if !strings.EqualFold(q.Name.String(), Service) {
			continue
		}
		if q.Type == dnsmessage.TypePTR || q.Type == dnsmessage.TypeALL {
			return true
		}
	}
	return false
}

// buildResponse packs the PTR answer plus the SRV, TXT and A records a
// client needs to resolve it without asking again.
func buildResponse(label string, port int, addr netip.Addr) ([]byte, error) {
	service, err := dnsmessage.NewName(Service)
	if err != nil {
		return nil, err
	}
	instance, err := dnsmessage.NewName(label + "." + Service)
	if err != nil {
		return nil, err
	}
	target, err := dnsmessage.NewName(label + ".local.")
	if err != nil {
		return nil, err
	}
	hdr := func(n dnsmessage.Name) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET, TTL: recordTTL}
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true, Authoritative: true},
		Answers: []dnsmessage.Resource{
			{Header: hdr(service), Body: &dnsmessage.PTRResource{PTR: instance}},
		},
		Additionals: []dnsmessage.Resource{
			{Header: hdr(instance), Body: &dnsmessage.SRVResource{Port: uint16(port), Target: target}},
			{Header: hdr(instance), Body: &dnsmessage.TXTResource{TXT: []string{"anvil=1"}}},
		},
	}
	if addr.Is4() {
		msg.Additionals = append(msg.Additionals, dnsmessage.Resource{
			Header: hdr(target), Body: &dnsmessage.AResource{A: addr.As4()},
		})
	}
	return msg.Pack()
}

// parseResponse pulls every advertised host out of one response. The address
// the packet came from wins, since that is the one known to be reachable; a
// host's own A record is only used when there is no source address.
func parseResponse(msg []byte, src netip.Addr) []Host {
	var m dnsmessage.Message
	if err := m.Unpack(msg); err != nil || !m.Header.Response {
		return nil
	}
	var labels []string
	ports := map[string]int{}
	targets := map[string]string{}
	addrs := map[string]netip.Addr{}

	for _, r := range append(append([]dnsmessage.Resource{}, m.Answers...), m.Additionals...) {
		switch body := r.Body.(type) {
		case *dnsmessage.PTRResource:
			if !strings.EqualFold(r.Header.Name.String(), Service) {
				continue
			}
			if l, ok := instanceLabel(body.PTR.String()); ok {
				labels = append(labels, l)
			}
		case *dnsmessage.SRVResource:
			if l, ok := instanceLabel(r.Header.Name.String()); ok {
				ports[l] = int(body.Port)
				targets[l] = strings.ToLower(body.Target.String())
			}
		case *dnsmessage.AResource:
			addrs[strings.ToLower(r.Header.Name.String())] = netip.AddrFrom4(body.A)
		}
	}

	out := make([]Host, 0, len(labels))
	for _, l := range labels {
		h := Host{Name: l, Addr: src, Port: ports[l]}
		if h.Port == 0 {
			h.Port = defaultSSH
		}
		if a, ok := addrs[targets[l]]; ok && !src.IsValid() {
			h.Addr = a
		}
		out = append(out, h)
	}
	return out
}

// instanceLabel returns the host part of "<label>._anvil._tcp.local.".
func instanceLabel(name string) (string, bool) {
	rest, ok := strings.CutSuffix(strings.ToLower(name), "."+Service)
	if !ok || rest == "" || strings.Contains(rest, ".") {
		return "", false
	}
	return rest, true
}

// hostLabel turns a hostname into one DNS label, defaulting to this host's own.
func hostLabel(name string) string {
	if name == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			h = "anvil"
		}
		name = h
	}
	name, _, _ = strings.Cut(name, ".")
	if l := dns.Label(name); l != "" {
		return l
	}
	return "anvil"
}

// listenMulticast binds the mDNS port with the reuse options that let anvild
// share it with an avahi-daemon already running on the host.
func listenMulticast() (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			for _, opt := range []int{unix.SO_REUSEADDR, unix.SO_REUSEPORT} {
				if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, opt, 1); err != nil {
					serr = err
				}
			}
		})
		if err != nil {
			return err
		}
		return serr
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", mdnsPort))
	if err != nil {
		return nil, fmt.Errorf("discovery: binding mDNS port: %w", err)
	}
	return conn.(*net.UDPConn), nil
}

// joinAll joins the mDNS group on every interface that can carry it. A
// failure on one interface leaves the others working.
func joinAll(p *ipv4.PacketConn) {
	group := &net.UDPAddr{IP: mdnsGroup}
	for _, ifi := range multicastInterfaces() {
		_ = p.JoinGroup(&ifi, group)
	}
}

func multicastInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagMulticast != 0 {
			out = append(out, ifi)
		}
	}
	return out
}

// interfaceAddr returns the first IPv4 address of an interface, so the A
// record points at the address the asker can actually reach.
func interfaceAddr(index int) netip.Addr {
	ifi, err := net.InterfaceByIndex(index)
	if err != nil {
		return netip.Addr{}
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(n.IP.To4()); ok && !ip.IsLoopback() {
			return ip
		}
	}
	return netip.Addr{}
}
