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
func fakeQMP(t *testing.T, greet bool, handle func(id, execute string) string) string {
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
			var cmd struct{ ID, Execute string }
			if json.Unmarshal(sc.Bytes(), &cmd) != nil {
				continue
			}
			fmt.Fprintln(conn, handle(cmd.ID, cmd.Execute))
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
	path := fakeQMP(t, true, func(id, _ string) string {
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

// TestBackupTargetNodeFitsQEMULimit covers a fork's job ID, which holds a full
// instance ID: QEMU rejects node names over 31 characters.
func TestBackupTargetNodeFitsQEMULimit(t *testing.T) {
	a := backupTargetNode("fork-01KXYZ0ABCDEFGHJKMNPQRSTVW")
	if len(a) > 31 {
		t.Errorf("node name %q is %d characters, QEMU allows 31", a, len(a))
	}
	if a == backupTargetNode("fork-01KXYZ0ABCDEFGHJKMNPQRSTVX") {
		t.Error("different jobs should get different node names")
	}
}

// TestFinishTopBackupCancelWaitsForTheJob covers a cancelled fork: QEMU only accepts
// job-dismiss once the cancelled job has concluded, which takes a few polls here.
func TestFinishTopBackupCancelWaitsForTheJob(t *testing.T) {
	var mu sync.Mutex
	status, aborting, dismissed := "running", 0, false
	path := fakeQMP(t, true, func(id, execute string) string {
		mu.Lock()
		defer mu.Unlock()
		ret := "{}"
		switch execute {
		case "query-jobs":
			if status == "aborting" {
				if aborting++; aborting == 3 {
					status = "concluded"
				}
			}
			ret = fmt.Sprintf(`[{"id":"job1","status":%q}]`, status)
		case "job-cancel":
			status = "aborting"
		case "job-dismiss":
			if status != "concluded" {
				return fmt.Sprintf(`{"error":{"class":"GenericError","desc":"job not concluded"},"id":%q}`, id)
			}
			dismissed = true
		}
		return fmt.Sprintf(`{"return":%s,"id":%q}`, ret, id)
	})
	c, err := DialQMP(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.FinishTopBackup(ctx, "job1", nil); err == nil {
		t.Fatal("expected the cancelled ctx's error")
	}
	mu.Lock()
	defer mu.Unlock()
	if !dismissed {
		t.Errorf("job dismissed before it concluded (status %s)", status)
	}
}
