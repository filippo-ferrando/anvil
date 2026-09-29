//go:build linux

package qemu

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeAgent serves a scripted subset of the qemu-guest-agent protocol on a unix socket.
type fakeAgent struct {
	socket   string
	files    map[string]string
	execOut  string
	execCode int
	noExec   bool
	shutdown chan struct{}
}

func startFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	a := &fakeAgent{
		socket:   filepath.Join(t.TempDir(), "qga.sock"),
		files:    map[string]string{},
		shutdown: make(chan struct{}, 1),
	}
	l, err := net.Listen("unix", a.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go a.serve(conn)
		}
	}()
	return a
}

func (a *fakeAgent) serve(conn net.Conn) {
	defer conn.Close()
	// Leftover garbage from an earlier client, which sync must skip.
	_, _ = conn.Write([]byte(`{"return": "stale"}` + "\n"))
	r := bufio.NewReader(conn)
	reply := func(v any) {
		data, _ := json.Marshal(map[string]any{"return": v})
		_, _ = conn.Write(append(data, '\n'))
	}
	fail := func(desc string) {
		data, _ := json.Marshal(map[string]any{"error": map[string]string{"class": "GenericError", "desc": desc}})
		_, _ = conn.Write(append(data, '\n'))
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			Execute   string          `json:"execute"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(line, &req)
		var args map[string]any
		_ = json.Unmarshal(req.Arguments, &args)
		switch req.Execute {
		case "guest-sync-delimited":
			_, _ = conn.Write([]byte{0xFF})
			reply(args["id"])
		case "guest-network-get-interfaces":
			reply([]map[string]any{
				{"name": "lo", "ip-addresses": []map[string]any{{"ip-address-type": "ipv4", "ip-address": "127.0.0.1", "prefix": 8}}},
				{"name": "eth0", "ip-addresses": []map[string]any{
					{"ip-address-type": "ipv4", "ip-address": "10.0.2.15", "prefix": 24},
					{"ip-address-type": "ipv6", "ip-address": "fe80::1", "prefix": 64},
				}},
			})
		case "guest-exec":
			if a.noExec {
				fail("Command guest-exec has been disabled")
				continue
			}
			reply(map[string]int{"pid": 42})
		case "guest-exec-status":
			reply(map[string]any{"exited": true, "exitcode": a.execCode, "out-data": base64.StdEncoding.EncodeToString([]byte(a.execOut))})
		case "guest-file-open":
			if _, ok := a.files[args["path"].(string)]; !ok {
				fail(fmt.Sprintf("failed to open file '%s' (mode: 'r'): No such file or directory", args["path"]))
				continue
			}
			reply(7)
		case "guest-file-read":
			var content string
			for _, c := range a.files {
				content = c
			}
			reply(map[string]any{"count": len(content), "buf-b64": base64.StdEncoding.EncodeToString([]byte(content)), "eof": true})
		case "guest-file-close":
			reply(map[string]any{})
		case "guest-shutdown":
			a.shutdown <- struct{}{} // no reply on success, like the real agent
		}
	}
}

func TestGuestAgentCommands(t *testing.T) {
	fa := startFakeAgent(t)
	fa.execOut = "status: done\n"
	fa.files["/run/cloud-init/result.json"] = `{"v1": {"errors": []}}`
	agent := NewGuestAgent(fa.socket)
	ctx := context.Background()

	err := agent.Do(ctx, func(c *QGAConn) error {
		ifaces, err := c.NetworkInterfaces()
		if err != nil {
			return err
		}
		if len(ifaces) != 2 || ifaces[1].IPAddresses[0].Address != "10.0.2.15" {
			return fmt.Errorf("unexpected interfaces: %+v", ifaces)
		}
		res, err := c.Exec(ctx, "cloud-init", "status")
		if err != nil {
			return err
		}
		if res.Stdout != "status: done\n" || res.ExitCode != 0 {
			return fmt.Errorf("unexpected exec result: %+v", res)
		}
		data, ok, err := c.ReadFile("/run/cloud-init/result.json")
		if err != nil || !ok || string(data) != fa.files["/run/cloud-init/result.json"] {
			return fmt.Errorf("ReadFile: %q %v %v", data, ok, err)
		}
		if _, ok, err := c.ReadFile("/missing"); ok || err != nil {
			return fmt.Errorf("expected a missing file to report ok=false without error, got ok=%v err=%v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGuestAgentExecDisabled(t *testing.T) {
	fa := startFakeAgent(t)
	fa.noExec = true
	err := NewGuestAgent(fa.socket).Do(context.Background(), func(c *QGAConn) error {
		_, err := c.Exec(context.Background(), "cloud-init", "status")
		return err
	})
	var qe *qgaError
	if !errors.As(err, &qe) {
		t.Fatalf("expected a guest agent error, got %v", err)
	}
}

func TestGuestAgentShutdownHasNoReply(t *testing.T) {
	fa := startFakeAgent(t)
	start := time.Now()
	err := NewGuestAgent(fa.socket).Do(context.Background(), func(c *QGAConn) error { return c.Shutdown() })
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-fa.shutdown:
	default:
		t.Fatal("expected the agent to receive guest-shutdown")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("Shutdown waited too long for a reply that never comes")
	}
}

func TestGuestAgentUnavailable(t *testing.T) {
	agent := NewGuestAgent(filepath.Join(t.TempDir(), "missing.sock"))
	if err := agent.Do(context.Background(), func(*QGAConn) error { return nil }); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("expected ErrAgentUnavailable for a missing socket, got %v", err)
	}

	// A socket nobody answers on, like QEMU's chardev while the guest has no agent running.
	sock := filepath.Join(t.TempDir(), "silent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := NewGuestAgent(sock).Do(ctx, func(*QGAConn) error { return nil }); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("expected ErrAgentUnavailable for a silent socket, got %v", err)
	}
}
