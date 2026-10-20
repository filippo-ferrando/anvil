package client

import "testing"

func TestParsePortMapping(t *testing.T) {
	p, err := ParsePortMapping("8080:80")
	if err != nil {
		t.Fatal(err)
	}
	if p.GetHostPort() != 8080 || p.GetGuestPort() != 80 || p.GetProtocol() != "" {
		t.Fatalf("got %+v", p)
	}

	p, err = ParsePortMapping("53:53/udp")
	if err != nil {
		t.Fatal(err)
	}
	if p.GetHostPort() != 53 || p.GetGuestPort() != 53 || p.GetProtocol() != "udp" {
		t.Fatalf("got %+v", p)
	}

	for _, bad := range []string{"8080", "8080-80", "x:80", "8080:x", "8080:80/sctp"} {
		if _, err := ParsePortMapping(bad); err == nil {
			t.Fatalf("ParsePortMapping(%q): expected an error", bad)
		}
	}
}

func TestParseHostPort(t *testing.T) {
	hostPort, protocol, err := ParseHostPort("8080")
	if err != nil {
		t.Fatal(err)
	}
	if hostPort != 8080 || protocol != "" {
		t.Fatalf("got (%d, %q)", hostPort, protocol)
	}

	hostPort, protocol, err = ParseHostPort("53/udp")
	if err != nil {
		t.Fatal(err)
	}
	if hostPort != 53 || protocol != "udp" {
		t.Fatalf("got (%d, %q)", hostPort, protocol)
	}

	for _, bad := range []string{"x", "8080/sctp"} {
		if _, _, err := ParseHostPort(bad); err == nil {
			t.Fatalf("ParseHostPort(%q): expected an error", bad)
		}
	}
}

func TestParseEnv(t *testing.T) {
	env, err := ParseEnv([]string{"A=1", "B=x=y", "C="})
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "x=y" || env["C"] != "" || len(env) != 3 {
		t.Fatalf("got %v", env)
	}
	if env, err := ParseEnv(nil); env != nil || err != nil {
		t.Fatalf("empty: %v, %v", env, err)
	}
	if _, err := ParseEnv([]string{"NOEQ"}); err == nil {
		t.Fatal("expected an error for a missing =")
	}
}

func TestParseVolumes(t *testing.T) {
	vols, err := ParseVolumes([]string{"/a:/b", "/c:/d:ro"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 || vols[0].GetReadOnly() || !vols[1].GetReadOnly() || vols[1].GetContainerPath() != "/d" {
		t.Fatalf("got %v", vols)
	}
	for _, bad := range []string{"/a", "/a:/b:rw", "/a:/b:ro:x"} {
		if _, err := ParseVolumes([]string{bad}); err == nil {
			t.Fatalf("ParseVolumes(%q): expected an error", bad)
		}
	}
}

func TestParsePorts(t *testing.T) {
	ports, err := ParsePorts([]string{"8080:80", "53:53/UDP"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || ports[1].GetProtocol() != "udp" {
		t.Fatalf("got %v", ports)
	}
	if _, err := ParsePorts([]string{"8080:80", "bad"}); err == nil {
		t.Fatal("expected an error for a bad entry")
	}
}
