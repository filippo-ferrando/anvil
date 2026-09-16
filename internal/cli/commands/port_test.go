package commands

import "testing"

func TestParsePortMapping(t *testing.T) {
	p, err := parsePortMapping("8080:80")
	if err != nil {
		t.Fatal(err)
	}
	if p.GetHostPort() != 8080 || p.GetGuestPort() != 80 || p.GetProtocol() != "" {
		t.Fatalf("got %+v", p)
	}

	p, err = parsePortMapping("53:53/udp")
	if err != nil {
		t.Fatal(err)
	}
	if p.GetHostPort() != 53 || p.GetGuestPort() != 53 || p.GetProtocol() != "udp" {
		t.Fatalf("got %+v", p)
	}

	for _, bad := range []string{"8080", "8080-80", "x:80", "8080:x", "8080:80/sctp"} {
		if _, err := parsePortMapping(bad); err == nil {
			t.Fatalf("parsePortMapping(%q): expected an error", bad)
		}
	}
}

func TestParseHostPort(t *testing.T) {
	hostPort, protocol, err := parseHostPort("8080")
	if err != nil {
		t.Fatal(err)
	}
	if hostPort != 8080 || protocol != "" {
		t.Fatalf("got (%d, %q)", hostPort, protocol)
	}

	hostPort, protocol, err = parseHostPort("53/udp")
	if err != nil {
		t.Fatal(err)
	}
	if hostPort != 53 || protocol != "udp" {
		t.Fatalf("got (%d, %q)", hostPort, protocol)
	}

	for _, bad := range []string{"x", "8080/sctp"} {
		if _, _, err := parseHostPort(bad); err == nil {
			t.Fatalf("parseHostPort(%q): expected an error", bad)
		}
	}
}
