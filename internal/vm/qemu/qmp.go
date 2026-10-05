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

// qmpWriteTimeout bounds a single command write, so a QEMU that has stopped
// reading its socket surfaces as an error instead of a hung caller.
const qmpWriteTimeout = 5 * time.Second

// QMPClient is a minimal client for QEMU's QMP protocol (newline-delimited
// JSON over a unix socket).
type QMPClient struct {
	conn   net.Conn
	reader *bufio.Reader

	mu      sync.Mutex // guards pending only, never held across I/O
	nextID  atomic.Int64
	pending map[string]chan qmpResponse

	writeMu sync.Mutex // serializes writers; readLoop must never wait on it
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

	// Read the greeting banner QEMU sends unprompted on connect. A QEMU that
	// accepts the socket but never greets would otherwise block Start forever.
	greetBy := time.Now().Add(qmpWriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(greetBy) {
		greetBy = d
	}
	_ = conn.SetReadDeadline(greetBy)
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

	_ = conn.SetReadDeadline(time.Time{})
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

	payload, err := json.Marshal(qmpCommand{Execute: command, Arguments: args, ID: id})
	if err != nil {
		return nil, fmt.Errorf("qmp: encoding command: %w", err)
	}
	payload = append(payload, '\n')

	c.mu.Lock()
	c.pending[id] = respCh
	c.mu.Unlock()

	// Written outside c.mu: a QEMU that has stopped draining the socket would
	// otherwise block readLoop too, and no response could ever be delivered.
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(qmpWriteTimeout))
	_, writeErr := c.conn.Write(payload)
	_ = c.conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
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

// AddVirtiofs hot-plugs a vhost-user-fs-pci device for tag, connected to the
// virtiofsd listening on socketPath, onto the first free hot-plug port.
func (c *QMPClient) AddVirtiofs(ctx context.Context, tag, socketPath string) error {
	chardev := VirtiofsChardevID(tag)
	_, err := c.Execute(ctx, "chardev-add", map[string]any{
		"id": chardev,
		"backend": map[string]any{
			"type": "socket",
			"data": map[string]any{
				"addr":   map[string]any{"type": "unix", "data": map[string]string{"path": socketPath}},
				"server": false,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("qmp: adding chardev for %s: %w", tag, err)
	}

	var lastErr error
	for i := range HotplugPorts {
		_, err := c.Execute(ctx, "device_add", map[string]any{
			"driver":     "vhost-user-fs-pci",
			"id":         VirtiofsDeviceID(tag),
			"chardev":    chardev,
			"tag":        tag,
			"queue-size": 1024,
			"bus":        HotplugPortID(i),
		})
		if err == nil {
			return nil
		}
		lastErr = err
		if !isPortTaken(err) {
			break
		}
	}
	_, _ = c.Execute(ctx, "chardev-remove", map[string]string{"id": chardev})
	return fmt.Errorf("qmp: hot-plugging virtiofs device %s: %w", tag, lastErr)
}

// isPortTaken reports whether device_add failed only because the chosen root port already holds a device.
func isPortTaken(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "occupied") || strings.Contains(msg, "in use") || strings.Contains(msg, "not available")
}

// RemoveVirtiofs unplugs tag's device and drops its chardev. The guest must release the
// device first (PCIe hot-unplug waits for the guest), so this polls until it is gone.
func (c *QMPClient) RemoveVirtiofs(ctx context.Context, tag string, timeout time.Duration) error {
	id := VirtiofsDeviceID(tag)
	if _, err := c.Execute(ctx, "device_del", map[string]string{"id": id}); err != nil {
		return fmt.Errorf("qmp: unplugging %s: %w", tag, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		present, err := c.hasPeripheral(ctx, id)
		if err != nil {
			return err
		}
		if !present {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("qmp: guest never released virtiofs device %s", tag)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if _, err := c.Execute(ctx, "chardev-remove", map[string]string{"id": VirtiofsChardevID(tag)}); err != nil {
		return fmt.Errorf("qmp: removing chardev for %s: %w", tag, err)
	}
	return nil
}

// hasPeripheral reports whether a device with this id is still attached.
func (c *QMPClient) hasPeripheral(ctx context.Context, id string) (bool, error) {
	raw, err := c.Execute(ctx, "qom-list", map[string]string{"path": "/machine/peripheral"})
	if err != nil {
		return false, fmt.Errorf("qmp: listing devices: %w", err)
	}
	var props []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &props); err != nil {
		return false, fmt.Errorf("qmp: decoding qom-list: %w", err)
	}
	for _, p := range props {
		if p.Name == id {
			return true, nil
		}
	}
	return false, nil
}

// DiskDriveID is the -drive id of every VM's main disk, as used by block commands.
const DiskDriveID = "disk0"

// StartTopBackup starts a point-in-time copy of drive's top layer into targetPath, an
// existing qcow2 already backed by the same base image. The copy reflects the disk as
// it was when this returns; call WaitJob with jobID for the copy to finish.
func (c *QMPClient) StartTopBackup(ctx context.Context, drive, targetPath, jobID string) error {
	node := jobID + "-target"
	if _, err := c.Execute(ctx, "blockdev-add", map[string]any{
		"driver":    "qcow2",
		"node-name": node,
		"file":      map[string]string{"driver": "file", "filename": targetPath},
	}); err != nil {
		return fmt.Errorf("qmp: opening backup target: %w", err)
	}
	if _, err := c.Execute(ctx, "blockdev-backup", map[string]any{
		"job-id":       jobID,
		"device":       drive,
		"target":       node,
		"sync":         "top",
		"auto-dismiss": false,
	}); err != nil {
		_, _ = c.Execute(ctx, "blockdev-del", map[string]string{"node-name": node})
		return fmt.Errorf("qmp: starting backup: %w", err)
	}
	return nil
}

// FinishTopBackup waits for a StartTopBackup job, then releases it and its target node.
// progress, if set, gets the job's completion percentage now and then.
func (c *QMPClient) FinishTopBackup(ctx context.Context, jobID string, progress func(pct int)) error {
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = c.Execute(cleanup, "job-dismiss", map[string]string{"id": jobID})
		_, _ = c.Execute(cleanup, "blockdev-del", map[string]string{"node-name": jobID + "-target"})
	}()
	lastPct := -1
	for {
		raw, err := c.Execute(ctx, "query-jobs", nil)
		if err != nil {
			return fmt.Errorf("qmp: querying jobs: %w", err)
		}
		var jobs []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Current  int64  `json:"current-progress"`
			Total    int64  `json:"total-progress"`
			ErrorMsg string `json:"error"`
		}
		if err := json.Unmarshal(raw, &jobs); err != nil {
			return fmt.Errorf("qmp: decoding query-jobs: %w", err)
		}
		found := false
		for _, j := range jobs {
			if j.ID != jobID {
				continue
			}
			found = true
			if j.Status == "concluded" {
				if j.ErrorMsg != "" {
					return fmt.Errorf("qmp: backup failed: %s", j.ErrorMsg)
				}
				return nil
			}
			if progress != nil && j.Total > 0 {
				if pct := int(j.Current * 100 / j.Total); pct != lastPct {
					progress(pct)
					lastPct = pct
				}
			}
		}
		if !found {
			return fmt.Errorf("qmp: backup job %s disappeared", jobID)
		}
		select {
		case <-ctx.Done():
			_, _ = c.Execute(context.WithoutCancel(ctx), "job-cancel", map[string]string{"id": jobID})
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// BlockResize grows drive's virtual size to sizeBytes while the VM runs.
func (c *QMPClient) BlockResize(ctx context.Context, drive string, sizeBytes int64) error {
	if _, err := c.Execute(ctx, "block_resize", map[string]any{"device": drive, "size": sizeBytes}); err != nil {
		return fmt.Errorf("qmp: resizing %s: %w", drive, err)
	}
	return nil
}

// HotpluggableCPU is one vCPU slot from query-hotpluggable-cpus. QOMPath is set when
// the slot holds a vCPU.
type HotpluggableCPU struct {
	Type  string `json:"type"`
	Props struct {
		SocketID int `json:"socket-id"`
		CoreID   int `json:"core-id"`
		ThreadID int `json:"thread-id"`
	} `json:"props"`
	QOMPath string `json:"qom-path"`
}

func (c *QMPClient) HotpluggableCPUs(ctx context.Context) ([]HotpluggableCPU, error) {
	raw, err := c.Execute(ctx, "query-hotpluggable-cpus", nil)
	if err != nil {
		return nil, fmt.Errorf("qmp: listing vCPU slots: %w", err)
	}
	var cpus []HotpluggableCPU
	if err := json.Unmarshal(raw, &cpus); err != nil {
		return nil, fmt.Errorf("qmp: decoding vCPU slots: %w", err)
	}
	return cpus, nil
}

// AddCPU plugs a vCPU into the empty slot.
func (c *QMPClient) AddCPU(ctx context.Context, slot HotpluggableCPU) error {
	_, err := c.Execute(ctx, "device_add", map[string]any{
		"driver":    slot.Type,
		"id":        fmt.Sprintf("vcpu-s%d", slot.Props.SocketID),
		"socket-id": slot.Props.SocketID,
		"core-id":   slot.Props.CoreID,
		"thread-id": slot.Props.ThreadID,
	})
	if err != nil {
		return fmt.Errorf("qmp: adding vCPU %d: %w", slot.Props.SocketID, err)
	}
	return nil
}

// RemoveCPU asks the guest to give up the vCPU at qomPath. It is gone once the guest
// has taken it offline, which the caller checks with HotpluggableCPUs.
func (c *QMPClient) RemoveCPU(ctx context.Context, qomPath string) error {
	if _, err := c.Execute(ctx, "device_del", map[string]string{"id": qomPath}); err != nil {
		return fmt.Errorf("qmp: removing vCPU %s: %w", qomPath, err)
	}
	return nil
}

// SetVirtioMemRequested asks the guest to hold sizeBytes in the virtio-mem device.
func (c *QMPClient) SetVirtioMemRequested(ctx context.Context, sizeBytes int64) error {
	_, err := c.Execute(ctx, "qom-set", map[string]any{
		"path": "/machine/peripheral/" + VirtioMemID, "property": "requested-size", "value": sizeBytes,
	})
	if err != nil {
		return fmt.Errorf("qmp: resizing virtio-mem: %w", err)
	}
	return nil
}

// VirtioMemSize is how much memory the guest currently holds in the virtio-mem device.
func (c *QMPClient) VirtioMemSize(ctx context.Context) (int64, error) {
	raw, err := c.Execute(ctx, "qom-get", map[string]string{"path": "/machine/peripheral/" + VirtioMemID, "property": "size"})
	if err != nil {
		return 0, fmt.Errorf("qmp: reading virtio-mem size: %w", err)
	}
	var size int64
	if err := json.Unmarshal(raw, &size); err != nil {
		return 0, fmt.Errorf("qmp: decoding virtio-mem size: %w", err)
	}
	return size, nil
}
