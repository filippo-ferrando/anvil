package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// QMPClient is a minimal hand-rolled client for QEMU's QMP protocol
// (newline-delimited JSON over a unix socket). Hand-rolled rather than a
// third-party dependency: the protocol is small, and this avoids taking on
// a library of uncertain maintenance status for something anvil only needs
// a handful of commands from (system_powerdown, quit, query-status,
// snapshot-save, snapshot-load).
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
			return
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.Event != "" {
			// Async events (e.g. SHUTDOWN) aren't consumed yet; the
			// supervisor loop (internal/instance/supervisor.go, M1
			// follow-up) will subscribe to these for crash detection
			// instead of relying solely on process-exit.
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
// wait for the VM to actually exit — the caller (internal/instance
// supervisor) is expected to watch the process/QMP socket and escalate to
// Quit after a timeout.
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

// SnapshotSave issues QMP's snapshot-save job. NOT YET WIRED to real
// device/vmstate identifiers — snapshot-save needs the vmstate-holding
// block device's node name and the list of block nodes to snapshot, both
// of which depend on how BuildArgs (args.go) names its -drive nodes; that
// naming isn't finalized yet, so this intentionally returns an error rather
// than sending a guessed, likely-wrong command. Wire this up alongside
// giving -drive stable node-name= values in args.go.
func (c *QMPClient) SnapshotSave(ctx context.Context, name string, timeout time.Duration) error {
	return fmt.Errorf("qmp: SnapshotSave not yet implemented (needs stable -drive node names from args.go)")
}

func (c *QMPClient) Close() error { return c.conn.Close() }
