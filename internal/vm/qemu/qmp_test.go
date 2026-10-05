//go:build linux

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

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

// fakeQMP serves a QMP socket. greet controls whether it sends the banner at
// all, so a silent QEMU can be simulated.
func fakeQMP(t *testing.T, greet bool, handle func(id string) string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if !greet {
			select {} // accept, then never speak
		}
		fmt.Fprintln(conn, `{"QMP":{"version":{}}}`)
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var cmd struct{ ID string }
			if json.Unmarshal(sc.Bytes(), &cmd) != nil {
				continue
			}
			fmt.Fprintln(conn, handle(cmd.ID))
		}
	}()
	return path
}

func TestDialQMPGivesUpOnASilentQEMU(t *testing.T) {
	path := fakeQMP(t, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := DialQMP(ctx, path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a QEMU that never greets")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DialQMP hung on a QEMU that accepted the socket but never greeted")
	}
}

func TestExecuteConcurrentCommandsEachGetTheirOwnReply(t *testing.T) {
	path := fakeQMP(t, true, func(id string) string {
		return fmt.Sprintf(`{"return":%q,"id":%q}`, "r"+id, id)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialQMP(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := c.Execute(ctx, "query-status", nil)
			if err != nil {
				errs[i] = err
				return
			}
			var got string
			if err := json.Unmarshal(raw, &got); err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("command %d: %v", i, err)
		}
	}
}
