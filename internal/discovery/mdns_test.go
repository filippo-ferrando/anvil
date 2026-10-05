package discovery

import (
	"net/netip"
	"testing"
)

func TestResponseRoundTrip(t *testing.T) {
	src := netip.MustParseAddr("192.168.1.9")
	msg, err := buildResponse("forge", 2222, netip.MustParseAddr("10.0.0.5"))
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	hosts := parseResponse(msg, src)
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1: %+v", len(hosts), hosts)
	}
	got := hosts[0]
	// The address answered from wins over the advertised A record, which may
	// be on a network the asker can't reach.
	want := Host{Name: "forge", Addr: src, Port: 2222}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestQueryIsForService(t *testing.T) {
	q, err := buildQuery()
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	if !wantsService(q) {
		t.Fatal("responder ignored its own browser's query")
	}
	resp, err := buildResponse("forge", 22, netip.Addr{})
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if wantsService(resp) {
		t.Fatal("a response was treated as a query, responders would answer each other")
	}
}

func TestHostLabel(t *testing.T) {
	if got := hostLabel("Forge.lan"); got != "forge" {
		t.Fatalf("hostLabel: got %q, want %q", got, "forge")
	}
	if got := hostLabel("!!"); got != "anvil" {
		t.Fatalf("hostLabel: got %q, want fallback %q", got, "anvil")
	}
}
