package client

import (
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCmdConn_RoundTrip(t *testing.T) {
	conn, err := startCmdConn(exec.Command("cat"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("got %q, %v", buf, err)
	}
}

func TestCmdConn_SurfacesStderr(t *testing.T) {
	conn, err := startCmdConn(exec.Command("sh", "-c", "echo 'Permission denied' >&2"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.ReadAll(conn)
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("want the ssh stderr in the error, got %v", err)
	}
}

func TestCmdConn_ReadAfterExit(t *testing.T) {
	conn, err := startCmdConn(exec.Command("sh", "-c", "echo hello; echo boom >&2"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(200 * time.Millisecond) // let the process exit before the first read
	data, err := io.ReadAll(conn)
	if string(data) != "hello\n" || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the output and then the stderr message, got %q, %v", data, err)
	}
}

func TestClient_RemoteHelpers(t *testing.T) {
	t.Setenv(muxEnv, "off") // the plain -J form; TestJumpArgs covers the shared connection
	local := &Client{}
	if local.jumpArgs() != nil || local.DockerEnv() != nil {
		t.Fatal("a local client must not touch ssh flags or the docker env")
	}
	if p, err := local.HostPath("rel"); err != nil || !strings.HasSuffix(p, "/rel") {
		t.Fatalf("local relative path: %q, %v", p, err)
	}

	remote := &Client{Remote: "me@box"}
	if got := remote.jumpArgs(); len(got) != 2 || got[1] != "me@box" {
		t.Fatalf("jumpArgs = %v", got)
	}
	env := remote.DockerEnv()
	if env[len(env)-1] != "DOCKER_HOST=ssh://me@box" {
		t.Fatalf("DockerEnv last = %q", env[len(env)-1])
	}
	if _, err := remote.HostPath("rel"); err == nil {
		t.Fatal("a relative path must be rejected for a remote daemon")
	}
	if p, err := remote.HostPath("/srv/data"); err != nil || p != "/srv/data" {
		t.Fatalf("remote absolute path: %q, %v", p, err)
	}
}

func TestSSHConfigHostname(t *testing.T) {
	out := []byte("user ops\nhostname 10.0.0.7\nport 22\nhostkeyalias box\n")
	if got := sshConfigHostname(out); got != "10.0.0.7" {
		t.Errorf("sshConfigHostname = %q, want 10.0.0.7", got)
	}
	if got := sshConfigHostname([]byte("user ops\n")); got != "" {
		t.Errorf("sshConfigHostname without a hostname line = %q", got)
	}
}

func TestDestHost(t *testing.T) {
	for dest, want := range map[string]string{
		"box":                   "box",
		"me@box":                "box",
		"ssh://me@box:2222":     "box",
		"ssh://me@[fe80::1]:22": "fe80::1",
		"ssh://box":             "box",
	} {
		if got := destHost(dest); got != want {
			t.Errorf("destHost(%q) = %q, want %q", dest, got, want)
		}
	}
}

func TestPickIP(t *testing.T) {
	ips := []net.IP{net.ParseIP("fe80::1"), net.ParseIP("192.168.1.5")}
	if got := pickIP(ips).String(); got != "192.168.1.5" {
		t.Errorf("pickIP = %s, want the IPv4 one", got)
	}
	if got := pickIP(ips[:1]).String(); got != "fe80::1" {
		t.Errorf("pickIP = %s, want fe80::1", got)
	}
}

func TestRemoteHostAddr_Local(t *testing.T) {
	if got := (&Client{}).RemoteHostAddr(t.Context()); got != "" {
		t.Errorf("local RemoteHostAddr = %q, want empty", got)
	}
}
