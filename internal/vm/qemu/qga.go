//go:build linux

package qemu

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// GuestAgentChannel is the virtio-serial port name qemu-guest-agent listens on.
const GuestAgentChannel = "org.qemu.guest_agent.0"

// ErrAgentUnavailable means no guest agent answered on the channel (not installed yet, or not running).
var ErrAgentUnavailable = errors.New("qga: guest agent not responding")

// qgaCallTimeout bounds each guest agent command, so a guest without an agent never hangs a caller.
const qgaCallTimeout = 3 * time.Second

// GuestAgent talks to qemu-guest-agent over the unix socket QEMU exposes for its
// virtio-serial channel. QEMU accepts one client at a time, so calls are serialized.
type GuestAgent struct {
	SocketPath string
	mu         sync.Mutex
}

func NewGuestAgent(socketPath string) *GuestAgent {
	return &GuestAgent{SocketPath: socketPath}
}

// QGAConn is one synced connection to the agent, valid inside a GuestAgent.Do callback.
type QGAConn struct {
	conn net.Conn
	r    *bufio.Reader
}

type qgaError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qgaError) Error() string { return fmt.Sprintf("qga: %s: %s", e.Class, e.Desc) }

type qgaResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *qgaError       `json:"error"`
}

// Do opens a connection, syncs it with guest-sync-delimited, and runs fn on it.
// Returns ErrAgentUnavailable when the agent doesn't answer the sync.
func (a *GuestAgent) Do(ctx context.Context, fn func(c *QGAConn) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := os.Stat(a.SocketPath); err != nil {
		return ErrAgentUnavailable
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", a.SocketPath)
	if err != nil {
		return ErrAgentUnavailable
	}
	defer conn.Close()
	// Unblock pending reads if the caller gives up.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	c := &QGAConn{conn: conn, r: bufio.NewReader(conn)}
	if err := c.sync(); err != nil {
		return ErrAgentUnavailable
	}
	return fn(c)
}

// sync flushes whatever an earlier client left half-read on the channel. The agent
// answers guest-sync-delimited with a 0xFF byte, then the id sent.
func (c *QGAConn) sync() error {
	id := rand.Int64N(1 << 50)
	if err := c.send("guest-sync-delimited", map[string]any{"id": id}); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(qgaCallTimeout))
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if b == 0xFF {
			break
		}
	}
	for {
		resp, err := c.readResponse()
		if err != nil {
			return err
		}
		var got int64
		if json.Unmarshal(resp.Return, &got) == nil && got == id {
			return nil
		}
	}
}

func (c *QGAConn) send(command string, args any) error {
	msg := map[string]any{"execute": command}
	if args != nil {
		msg["arguments"] = args
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(qgaCallTimeout))
	_, err = c.conn.Write(append(data, '\n'))
	return err
}

func (c *QGAConn) readResponse() (qgaResponse, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return qgaResponse{}, err
	}
	// A stray 0xFF from an earlier sync may precede the JSON.
	line = []byte(strings.TrimLeft(string(line), "\xff"))
	var resp qgaResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return qgaResponse{}, fmt.Errorf("qga: decoding response %q: %w", line, err)
	}
	return resp, nil
}

// call runs command and decodes its return value into out (skipped if out is nil).
func (c *QGAConn) call(command string, args, out any) error {
	return c.callWithTimeout(command, args, out, qgaCallTimeout)
}

func (c *QGAConn) callWithTimeout(command string, args, out any, timeout time.Duration) error {
	if err := c.send(command, args); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	resp, err := c.readResponse()
	if err != nil {
		return fmt.Errorf("qga: %s: %w", command, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Return, out); err != nil {
		return fmt.Errorf("qga: decoding %s reply: %w", command, err)
	}
	return nil
}

// GuestInterface is one network interface as reported by guest-network-get-interfaces.
type GuestInterface struct {
	Name        string `json:"name"`
	HWAddress   string `json:"hardware-address"`
	IPAddresses []struct {
		Type    string `json:"ip-address-type"` // "ipv4" | "ipv6"
		Address string `json:"ip-address"`
		Prefix  int    `json:"prefix"`
	} `json:"ip-addresses"`
}

func (c *QGAConn) NetworkInterfaces() ([]GuestInterface, error) {
	var ifaces []GuestInterface
	err := c.call("guest-network-get-interfaces", nil, &ifaces)
	return ifaces, err
}

// fsfreezeTimeout bounds a freeze, which first flushes every guest filesystem.
const fsfreezeTimeout = 30 * time.Second

// FreezeFilesystems flushes and freezes every guest filesystem. Always pair it with
// ThawFilesystems: guest writes block until then.
func (c *QGAConn) FreezeFilesystems() error {
	return c.callWithTimeout("guest-fsfreeze-freeze", nil, nil, fsfreezeTimeout)
}

// ThawFilesystems undoes FreezeFilesystems.
func (c *QGAConn) ThawFilesystems() error {
	return c.callWithTimeout("guest-fsfreeze-thaw", nil, nil, fsfreezeTimeout)
}

// OnlineAllCPUs brings every offline guest vCPU online (a hot-plugged one may start
// offline) and returns how many it changed.
func (c *QGAConn) OnlineAllCPUs() (int, error) {
	var vcpus []struct {
		LogicalID int  `json:"logical-id"`
		Online    bool `json:"online"`
	}
	if err := c.call("guest-get-vcpus", nil, &vcpus); err != nil {
		return 0, err
	}
	var offline []map[string]any
	for _, v := range vcpus {
		if !v.Online {
			offline = append(offline, map[string]any{"logical-id": v.LogicalID, "online": true})
		}
	}
	if len(offline) == 0 {
		return 0, nil
	}
	var changed int
	err := c.call("guest-set-vcpus", map[string]any{"vcpus": offline}, &changed)
	return changed, err
}

// OnlineAllMemory brings every offline guest memory block online, for memory the
// guest added but its distro doesn't online by itself.
func (c *QGAConn) OnlineAllMemory() error {
	var blocks []struct {
		Phys     int64 `json:"phys-index"`
		Online   bool  `json:"online"`
		CanOffln bool  `json:"can-offline"`
	}
	if err := c.call("guest-get-memory-blocks", nil, &blocks); err != nil {
		return err
	}
	var offline []map[string]any
	for _, b := range blocks {
		if !b.Online {
			offline = append(offline, map[string]any{"phys-index": b.Phys, "online": true})
		}
	}
	if len(offline) == 0 {
		return nil
	}
	return c.call("guest-set-memory-blocks", map[string]any{"mem-blks": offline}, nil)
}

// Shutdown asks the guest OS to power off. The agent sends no reply when this
// works, only an error reply when it doesn't.
func (c *QGAConn) Shutdown() error {
	if err := c.send("guest-shutdown", map[string]string{"mode": "powerdown"}); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := c.readResponse()
	if err != nil {
		return nil // no reply (timeout or closed channel): shutdown is under way
	}
	if resp.Error != nil {
		return resp.Error
	}
	return nil
}

// ExecResult is the outcome of a program run in the guest via Exec.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Exec runs path with args in the guest and waits for it to finish.
func (c *QGAConn) Exec(ctx context.Context, path string, args ...string) (ExecResult, error) {
	var started struct {
		PID int `json:"pid"`
	}
	if err := c.call("guest-exec", map[string]any{"path": path, "arg": args, "capture-output": true}, &started); err != nil {
		return ExecResult{}, err
	}
	for {
		var st struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		}
		if err := c.call("guest-exec-status", map[string]int{"pid": started.PID}, &st); err != nil {
			return ExecResult{}, err
		}
		if st.Exited {
			out, _ := base64.StdEncoding.DecodeString(st.OutData)
			errOut, _ := base64.StdEncoding.DecodeString(st.ErrData)
			return ExecResult{ExitCode: st.ExitCode, Stdout: string(out), Stderr: string(errOut)}, nil
		}
		select {
		case <-ctx.Done():
			return ExecResult{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ReadFile reads a whole (small) file from the guest. ok is false if it doesn't exist.
func (c *QGAConn) ReadFile(path string) (data []byte, ok bool, err error) {
	var handle int
	if err := c.call("guest-file-open", map[string]string{"path": path, "mode": "r"}, &handle); err != nil {
		var qe *qgaError
		if errors.As(err, &qe) && strings.Contains(strings.ToLower(qe.Desc), "no such file") {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = c.call("guest-file-close", map[string]int{"handle": handle}, nil) }()
	for {
		var chunk struct {
			Count int    `json:"count"`
			Buf   string `json:"buf-b64"`
			EOF   bool   `json:"eof"`
		}
		if err := c.call("guest-file-read", map[string]int{"handle": handle, "count": 64 << 10}, &chunk); err != nil {
			return nil, false, err
		}
		b, err := base64.StdEncoding.DecodeString(chunk.Buf)
		if err != nil {
			return nil, false, fmt.Errorf("qga: decoding file data: %w", err)
		}
		data = append(data, b...)
		if chunk.EOF || chunk.Count == 0 || len(data) > 1<<20 {
			return data, true, nil
		}
	}
}
