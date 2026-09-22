package dns

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 42, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func parse(t *testing.T, resp []byte) dnsmessage.Message {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("unpacking response: %v", err)
	}
	return m
}

func testZone() Zone {
	return Zone{
		Domain:  "myapp.anvil",
		Listen:  netip.MustParseAddr("127.0.0.1"),
		Subnet:  netip.MustParsePrefix("10.55.201.0/24"),
		Records: map[string]netip.Addr{"db": netip.MustParseAddr("10.55.201.2")},
	}
}

func TestHandleAnswersKnownMember(t *testing.T) {
	s := NewServer(nil)
	m := parse(t, s.handle(testZone(), query(t, "DB.myapp.anvil.", dnsmessage.TypeA), "udp"))
	if m.Header.ID != 42 || !m.Header.Authoritative || m.Header.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("unexpected header %+v", m.Header)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("expected one answer, got %d", len(m.Answers))
	}
	a, ok := m.Answers[0].Body.(*dnsmessage.AResource)
	if !ok || netip.AddrFrom4(a.A) != netip.MustParseAddr("10.55.201.2") {
		t.Errorf("expected A 10.55.201.2, got %v", m.Answers[0].Body)
	}
	if m.Answers[0].Header.TTL != recordTTL {
		t.Errorf("expected TTL %d, got %d", recordTTL, m.Answers[0].Header.TTL)
	}
}

func TestHandleUnknownMemberIsNXDomain(t *testing.T) {
	s := NewServer(nil)
	m := parse(t, s.handle(testZone(), query(t, "cache.myapp.anvil.", dnsmessage.TypeA), "udp"))
	if m.Header.RCode != dnsmessage.RCodeNameError || len(m.Answers) != 0 {
		t.Errorf("expected NXDOMAIN with no answers, got %+v", m)
	}
}

func TestHandleAAAAIsEmptySuccess(t *testing.T) {
	s := NewServer(nil)
	m := parse(t, s.handle(testZone(), query(t, "db.myapp.anvil.", dnsmessage.TypeAAAA), "udp"))
	if m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("expected an empty success for a name with no AAAA record, got %+v", m)
	}
}

func TestHandleNoUpstreamIsServFail(t *testing.T) {
	s := NewServer(nil)
	m := parse(t, s.handle(testZone(), query(t, "example.com.", dnsmessage.TypeA), "udp"))
	if m.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("expected SERVFAIL with no upstreams, got %v", m.Header.RCode)
	}
}

func TestHandleForwardsOutsideZone(t *testing.T) {
	upstream, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		buf := make([]byte, 512)
		n, from, err := upstream.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		hdr, _ := p.Start(buf[:n])
		q, _ := p.Question()
		addr := netip.MustParseAddr("93.184.216.34")
		_, _ = upstream.WriteToUDP(reply(hdr, &q, dnsmessage.RCodeSuccess, &addr), from)
	}()

	s := NewServer(nil)
	s.upstreams = []string{upstream.LocalAddr().String()}
	m := parse(t, s.handle(testZone(), query(t, "example.com.", dnsmessage.TypeA), "udp"))
	if len(m.Answers) != 1 {
		t.Fatalf("expected the upstream's answer to be relayed, got %+v", m)
	}
}

func TestAllowed(t *testing.T) {
	z := testZone()
	for addr, want := range map[string]bool{
		"10.55.201.9": true,
		"127.0.0.1":   true,
		"192.168.1.5": false,
	} {
		if got := allowed(z, netip.MustParseAddr(addr)); got != want {
			t.Errorf("allowed(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"db":         "db",
		"Web_Server": "web-server",
		"-api-":      "api",
		"my.app":     "my-app",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Domain("MyApp"); got != "myapp.anvil" {
		t.Errorf("Domain = %q", got)
	}
}

func TestReadUpstreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	content := "# comment\nnameserver 127.0.0.53\nnameserver ::1\noptions edns0\nnameserver bogus\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readUpstreams(path)
	if len(got) != 2 || got[0] != "127.0.0.53:53" || got[1] != "[::1]:53" {
		t.Errorf("unexpected upstreams %v", got)
	}
}

// freePort returns a port currently free for both UDP and TCP on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err == nil {
			u.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

func exchangeWith(t *testing.T, network string, port int, name string) dnsmessage.Message {
	t.Helper()
	resp, err := exchange(network, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), query(t, name, dnsmessage.TypeA))
	if err != nil {
		t.Fatalf("%s exchange: %v", network, err)
	}
	return parse(t, resp)
}

func TestRefreshServesAndPicksUpChanges(t *testing.T) {
	zone := testZone()
	s := NewServer(func(context.Context) ([]Zone, error) { return []Zone{zone}, nil })
	s.Port = freePort(t)
	s.ResolvConf = filepath.Join(t.TempDir(), "missing")
	defer s.Close()

	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	for _, network := range []string{"udp", "tcp"} {
		if m := exchangeWith(t, network, s.Port, "db.myapp.anvil."); len(m.Answers) != 1 {
			t.Errorf("%s: expected db to resolve, got %+v", network, m)
		}
	}

	// A member added after the zone went up resolves after the next refresh.
	zone.Records = map[string]netip.Addr{
		"db":    netip.MustParseAddr("10.55.201.2"),
		"cache": netip.MustParseAddr("10.55.201.130"),
	}
	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if m := exchangeWith(t, "udp", s.Port, "cache.myapp.anvil."); len(m.Answers) != 1 {
		t.Errorf("expected the new member to resolve, got %+v", m)
	}
}

func TestRefreshClosesRemovedZones(t *testing.T) {
	zones := []Zone{testZone()}
	s := NewServer(func(context.Context) ([]Zone, error) { return zones, nil })
	s.Port = freePort(t)
	defer s.Close()

	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	zones = nil
	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(s.listeners) != 0 {
		t.Errorf("expected no listeners left, got %d", len(s.listeners))
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)), time.Second)
	if err == nil {
		conn.Close()
		t.Error("expected the TCP listener to be closed")
	}
}
