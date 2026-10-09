package client

import (
	"io"
	"os/exec"
	"strings"
	"testing"
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

func TestClient_RemoteHelpers(t *testing.T) {
	local := &Client{}
	if local.JumpArgs() != nil || local.DockerEnv() != nil {
		t.Fatal("a local client must not touch ssh flags or the docker env")
	}
	if p, err := local.HostPath("rel"); err != nil || !strings.HasSuffix(p, "/rel") {
		t.Fatalf("local relative path: %q, %v", p, err)
	}

	remote := &Client{Remote: "me@box"}
	if got := remote.JumpArgs(); len(got) != 2 || got[1] != "me@box" {
		t.Fatalf("JumpArgs = %v", got)
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
