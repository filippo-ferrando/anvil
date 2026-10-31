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
	"syscall"
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
		args := append([]string{"-T", "-o", "BatchMode=yes"}, muxArgs()...)
		args = append(args, dest, "anvil", "dial-stdio", "--socket", socketPath)
		return startCmdConn(exec.Command(sshBin, args...))
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

// jumpArgs returns ssh flags that reach a guest through the daemon's host, nil when local.
// The hop shares the remote host's connection: -J would start one of its own.
func (c *Client) jumpArgs() []string {
	if c.Remote == "" {
		return nil
	}
	mux := muxArgs()
	if mux == nil {
		return []string{"-J", c.Remote}
	}
	hop := []string{"ssh"}
	for _, a := range mux {
		// ssh expands %-tokens in a ProxyCommand: %% leaves the inner ssh its own %C.
		hop = append(hop, shQuote(strings.ReplaceAll(a, "%", "%%")))
	}
	hop = append(hop, "-W", "%h:%p", shQuote(strings.ReplaceAll(c.Remote, "%", "%%")))
	return []string{"-o", "ProxyCommand=" + strings.Join(hop, " ")}
}

// muxEnv set to "off" gives every ssh call to the remote host its own connection.
const muxEnv = "ANVIL_SSH_MUX"

// muxArgs makes the ssh calls to one remote host share one connection, kept for 60s after
// the last one, so each command doesn't pay a new handshake. Nil when turned off, or when
// there is no private dir for the control socket.
func muxArgs() []string {
	if os.Getenv(muxEnv) == "off" {
		return nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("anvil-ssh-%d", os.Getuid()))
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return nil
		}
		// In a shared /tmp, only a dir of our own, closed to others, is safe.
		if st, err := os.Lstat(dir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 || !ownedByMe(st) {
			return nil
		}
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + filepath.Join(dir, "anvil-ssh-%C"),
		"-o", "ControlPersist=60s",
	}
}

// shQuote quotes s for sh, which runs a ProxyCommand.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// ownedByMe reports whether st belongs to the current user.
func ownedByMe(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}
