package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialSSH connects to the anvild on dest ("[user@]host" or "ssh://[user@]host[:port]")
// by running `anvil dial-stdio` there over ssh. socketPath is the socket on dest.
func DialSSH(dest, socketPath string) (*Client, error) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("client: ssh not found on PATH (needed for --remote)")
	}
	dialer := func(context.Context, string) (net.Conn, error) {
		// BatchMode: a password prompt can't work behind gRPC, key or agent auth is required.
		return startCmdConn(exec.Command(sshBin, "-T", "-o", "BatchMode=yes", dest,
			"anvil", "dial-stdio", "--socket", socketPath))
	}
	conn, err := grpc.NewClient("passthrough:///anvild",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, fmt.Errorf("client: dialing %s over ssh: %w", dest, err)
	}
	c := newClient(conn)
	c.Remote = dest
	return c, nil
}

// JumpArgs returns ssh flags that reach a guest through the daemon's host, nil when local.
func (c *Client) JumpArgs() []string {
	if c.Remote == "" {
		return nil
	}
	return []string{"-J", c.Remote}
}

// DockerEnv returns the environment for a docker CLI call that must reach the daemon's host.
func (c *Client) DockerEnv() []string {
	if c.Remote == "" {
		return nil // nil keeps exec.Cmd's default, the current environment
	}
	dest := c.Remote
	if !strings.HasPrefix(dest, "ssh://") {
		dest = "ssh://" + dest
	}
	return append(os.Environ(), "DOCKER_HOST="+dest)
}

// HostPath turns p into an absolute path on the daemon's host. A remote path
// can't be resolved locally, so it must already be absolute.
func (c *Client) HostPath(p string) (string, error) {
	if c.Remote == "" {
		return filepath.Abs(p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s: path must be absolute on the remote host %s", p, c.Remote)
	}
	return p, nil
}

// cmdConn is a net.Conn over a child process's stdin and stdout.
type cmdConn struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.ReadCloser
	stderr *bytes.Buffer
	done   chan struct{} // closed once the process has exited and stderr is complete
	once   sync.Once
}

func startCmdConn(cmd *exec.Cmd) (net.Conn, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &cmdConn{cmd: cmd, in: in, out: out, stderr: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *cmdConn) Read(b []byte) (int, error) {
	n, err := c.out.Read(b)
	if err == io.EOF {
		// Surface ssh's own complaint (auth, unknown host, no anvil) instead of a bare EOF.
		<-c.done
		if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
			return n, fmt.Errorf("ssh: %s", msg)
		}
	}
	return n, err
}

func (c *cmdConn) Write(b []byte) (int, error) { return c.in.Write(b) }

func (c *cmdConn) Close() error {
	c.once.Do(func() {
		c.in.Close()
		_ = c.cmd.Process.Kill()
	})
	return nil
}

func (c *cmdConn) LocalAddr() net.Addr              { return cmdAddr{} }
func (c *cmdConn) RemoteAddr() net.Addr             { return cmdAddr{} }
func (c *cmdConn) SetDeadline(time.Time) error      { return nil }
func (c *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cmdConn) SetWriteDeadline(time.Time) error { return nil }

type cmdAddr struct{}

func (cmdAddr) Network() string { return "ssh" }
func (cmdAddr) String() string  { return "ssh" }
