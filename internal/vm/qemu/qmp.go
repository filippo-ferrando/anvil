//go:build linux

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QMPClient is a minimal client for QEMU's QMP protocol (newline-delimited
// JSON over a unix socket).
type QMPClient struct {
	conn   net.Conn
	reader *bufio.Reader

	mu      sync.Mutex // serializes command/response round-trips
	nextID  atomic.Int64
	pending map[string]chan qmpResponse
}

type qmpGreeting struct {
	QMP struct {
		Version json.RawMessage `json:"version"`
	} `json:"QMP"`
}

type qmpCommand struct {
	Execute   string      `json:"execute"`
	Arguments interface{} `json:"arguments,omitempty"`
	ID        string      `json:"id,omitempty"`
}

type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *qmpError       `json:"error"`
	ID     string          `json:"id"`
	Event  string          `json:"event"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qmpError) Error() string { return fmt.Sprintf("qmp: %s: %s", e.Class, e.Desc) }

// DialQMP connects to a running instance's QMP unix socket and completes the
// capabilities-negotiation handshake QEMU requires before accepting commands.
func DialQMP(ctx context.Context, socketPath string) (*QMPClient, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("qmp: dial %s: %w", socketPath, err)
	}

	c := &QMPClient{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		pending: make(map[string]chan qmpResponse),
	}

	// Read the greeting banner QEMU sends unprompted on connect.
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp: reading greeting: %w", err)
	}
	var greeting qmpGreeting
	if err := json.Unmarshal(line, &greeting); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp: parsing greeting: %w", err)
	}

	go c.readLoop()

	if _, err := c.Execute(ctx, "qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp: capabilities handshake: %w", err)
	}
	return c, nil
}

func (c *QMPClient) readLoop() {
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			// Connection gone: fail every outstanding command.
			c.failPending(fmt.Errorf("connection closed: %w", err))
			return
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.Event != "" {
			// Async events aren't consumed yet.
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// failPending delivers err to every command still waiting on a response.
func (c *QMPClient) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan qmpResponse)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- qmpResponse{Error: &qmpError{Class: "internal", Desc: err.Error()}}
	}
}

// Execute sends a QMP command and waits for its response.
func (c *QMPClient) Execute(ctx context.Context, command string, args interface{}) (json.RawMessage, error) {
	id := fmt.Sprintf("%d", c.nextID.Add(1))
	respCh := make(chan qmpResponse, 1)

	c.mu.Lock()
	c.pending[id] = respCh
	cmd := qmpCommand{Execute: command, Arguments: args, ID: id}
	payload, err := json.Marshal(cmd)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("qmp: encoding command: %w", err)
	}
	payload = append(payload, '\n')
	_, writeErr := c.conn.Write(payload)
	c.mu.Unlock()
	if writeErr != nil {
		// No response will ever arrive for this id; clean up the entry.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("qmp: writing command: %w", writeErr)
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Return, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// GracefulShutdown asks the guest OS to power down via ACPI. It does not
// wait for the VM to actually exit.
func (c *QMPClient) GracefulShutdown(ctx context.Context) error {
	_, err := c.Execute(ctx, "system_powerdown", nil)
	return err
}

// Quit forcibly terminates the QEMU process.
func (c *QMPClient) Quit(ctx context.Context) error {
	_, err := c.Execute(ctx, "quit", nil)
	return err
}

type QueryStatusResult struct {
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

func (c *QMPClient) QueryStatus(ctx context.Context) (QueryStatusResult, error) {
	raw, err := c.Execute(ctx, "query-status", nil)
	if err != nil {
		return QueryStatusResult{}, err
	}
	var res QueryStatusResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return QueryStatusResult{}, fmt.Errorf("qmp: decoding query-status: %w", err)
	}
	return res, nil
}

// humanMonitorCommand runs an arbitrary HMP command line through QMP's
// human-monitor-command passthrough, returning its plain-text output.
func (c *QMPClient) humanMonitorCommand(ctx context.Context, line string) (string, error) {
	raw, err := c.Execute(ctx, "human-monitor-command", map[string]string{"command-line": line})
	if err != nil {
		return "", err
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("qmp: decoding human-monitor-command output: %w", err)
	}
	return out, nil
}

// AddHostForward adds a live SLIRP host-to-guest port forward via hostfwd_add.
// HMP reports failure as plain text, not a QMP error, so non-empty output here means failure.
func (c *QMPClient) AddHostForward(ctx context.Context, netdevID string, f HostForward) error {
	out, err := c.humanMonitorCommand(ctx, hostfwdAddLine(netdevID, f))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("qmp: hostfwd_add: %s", strings.TrimSpace(out))
	}
	return nil
}

// RemoveHostForward removes a forward added by AddHostForward, via hostfwd_remove.
// Unlike hostfwd_add, success is confirmed by wording like "...removed", not by empty output.
func (c *QMPClient) RemoveHostForward(ctx context.Context, netdevID string, hostPort int, protocol string) error {
	out, err := c.humanMonitorCommand(ctx, hostfwdRemoveLine(netdevID, hostPort, protocol))
	if err != nil {
		return err
	}
	out = strings.TrimSpace(out)
	if isHostfwdRemoveSuccess(out) {
		return nil
	}
	return fmt.Errorf("qmp: hostfwd_remove: %s", out)
}

// isHostfwdRemoveSuccess reports whether out (hostfwd_remove's already-trimmed
// HMP output) indicates the rule was actually removed.
func isHostfwdRemoveSuccess(out string) bool {
	return out == "" || strings.Contains(out, "removed")
}

// SaveVM creates or overwrites a named live internal snapshot via the savevm HMP command.
// Returns raw output uninterpreted since HMP wording isn't reliably parseable; the caller verifies via the disk's own snapshot table instead.
func (c *QMPClient) SaveVM(ctx context.Context, name string) (string, error) {
	return c.humanMonitorCommand(ctx, "savevm "+name)
}

// DeleteVMSnapshot removes a named internal snapshot via delvm, live (qemu-img
// can't touch a disk file a running QEMU process holds locked). See SaveVM's doc on the raw output.
func (c *QMPClient) DeleteVMSnapshot(ctx context.Context, name string) (string, error) {
	return c.humanMonitorCommand(ctx, "delvm "+name)
}

// hostfwdAddLine renders the hostfwd_add HMP command line for f on netdevID.
// The empty host/guest addresses (::) mean "any host" / "the guest's own address", the same convention buildNetdev's hostfwd= option uses.
func hostfwdAddLine(netdevID string, f HostForward) string {
	proto := f.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return fmt.Sprintf("hostfwd_add %s %s::%d-:%d", netdevID, proto, f.HostPort, f.GuestPort)
}

// hostfwdRemoveLine renders the hostfwd_remove HMP command line.
func hostfwdRemoveLine(netdevID string, hostPort int, protocol string) string {
	if protocol == "" {
		protocol = "tcp"
	}
	return fmt.Sprintf("hostfwd_remove %s %s::%d", netdevID, protocol, hostPort)
}

// SnapshotSave issues QMP's snapshot-save job. Not yet implemented.
func (c *QMPClient) SnapshotSave(ctx context.Context, name string, timeout time.Duration) error {
	return fmt.Errorf("qmp: SnapshotSave not yet implemented (needs stable -drive node names from args.go)")
}

func (c *QMPClient) Close() error { return c.conn.Close() }
