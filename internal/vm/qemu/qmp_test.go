//go:build linux

package qemu

import "testing"

func TestHostfwdAddLine(t *testing.T) {
	cases := []struct {
		name string
		f    HostForward
		want string
	}{
		{"defaults to tcp", HostForward{HostPort: 8080, GuestPort: 80}, "hostfwd_add net0 tcp::8080-:80"},
		{"explicit udp", HostForward{HostPort: 53, GuestPort: 53, Protocol: "udp"}, "hostfwd_add net0 udp::53-:53"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hostfwdAddLine("net0", c.f); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestHostfwdRemoveLine(t *testing.T) {
	cases := []struct {
		name     string
		hostPort int
		protocol string
		want     string
	}{
		{"defaults to tcp", 8080, "", "hostfwd_remove net0 tcp::8080"},
		{"explicit udp", 53, "udp", "hostfwd_remove net0 udp::53"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hostfwdRemoveLine("net0", c.hostPort, c.protocol); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestIsHostfwdRemoveSuccess guards against the exact bug that shipped:
// treating QEMU's plain-text success confirmation as a failure, since hostfwd_remove reports success as text rather than staying silent like hostfwd_add.
func TestIsHostfwdRemoveSuccess(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"", true},
		{"host forwarding rule for tcp::7000 removed", true},
		{"host forwarding rule for udp::53 removed", true},
		{"invalid host forwarding rule", false},
		{"could not remove host forwarding rule", false},
	}
	for _, c := range cases {
		if got := isHostfwdRemoveSuccess(c.out); got != c.want {
			t.Errorf("isHostfwdRemoveSuccess(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}
