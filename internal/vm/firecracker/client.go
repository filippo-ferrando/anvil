//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiCallTimeout bounds one API call, so a stuck VMM never hangs a caller.
// Snapshot calls write or map the whole guest memory, so they get snapshotCallTimeout.
const (
	apiCallTimeout      = 10 * time.Second
	snapshotCallTimeout = 5 * time.Minute
)

// Client talks to one Firecracker process over its API unix socket.
type Client struct {
	SocketPath string
	http       *http.Client
	slow       *http.Client
}

func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		SocketPath: socketPath,
		http:       &http.Client{Timeout: apiCallTimeout, Transport: transport},
		slow:       &http.Client{Timeout: snapshotCallTimeout, Transport: transport},
	}
}

// APIError is Firecracker refusing a request, with the fault message it sent back.
type APIError struct {
	Status int
	Fault  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker: API returned %d: %s", e.Status, e.Fault)
}

// do sends body as JSON (if not nil) and decodes a JSON reply into out (if not nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWith(ctx, c.http, method, path, body, out)
}

func (c *Client) doWith(ctx context.Context, hc *http.Client, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("firecracker: encoding %s %s: %w", method, path, err)
		}
		r = bytes.NewReader(data)
	}
	// The host part is ignored: the transport always dials the unix socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("firecracker: reading reply to %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		var fault struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.Unmarshal(data, &fault)
		if fault.FaultMessage == "" {
			fault.FaultMessage = string(data)
		}
		return &APIError{Status: resp.StatusCode, Fault: fault.FaultMessage}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("firecracker: decoding reply to %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Instance states reported by Info.
const (
	StateNotStarted = "Not started"
	StateRunning    = "Running"
	StatePaused     = "Paused"
)

// InstanceInfo is GET /.
type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	VMMVersion string `json:"vmm_version"`
}

func (c *Client) Info(ctx context.Context) (InstanceInfo, error) {
	var info InstanceInfo
	err := c.do(ctx, http.MethodGet, "/", nil, &info)
	return info, err
}

func (c *Client) action(ctx context.Context, action string) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": action}, nil)
}

// SendCtrlAltDel asks the guest to shut down (x86_64 only). Firecracker exits once
// the guest reboots, so this ends the VM.
func (c *Client) SendCtrlAltDel(ctx context.Context) error {
	return c.action(ctx, "SendCtrlAltDel")
}

func (c *Client) setState(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": state}, nil)
}

func (c *Client) Pause(ctx context.Context) error  { return c.setState(ctx, "Paused") }
func (c *Client) Resume(ctx context.Context) error { return c.setState(ctx, "Resumed") }

// UpdateDrive points a drive at pathOnHost again, which also makes the guest see the
// file's new size after it grew.
func (c *Client) UpdateDrive(ctx context.Context, driveID, pathOnHost string) error {
	return c.do(ctx, http.MethodPatch, "/drives/"+driveID,
		map[string]string{"drive_id": driveID, "path_on_host": pathOnHost}, nil)
}

// MemoryHotplugStatus is GET /hotplug/memory: the virtio-mem region's size and how much
// of it the guest has plugged.
type MemoryHotplugStatus struct {
	TotalSizeMiB     int64 `json:"total_size_mib"`
	PluggedSizeMiB   int64 `json:"plugged_size_mib"`
	RequestedSizeMiB int64 `json:"requested_size_mib"`
}

func (c *Client) MemoryHotplug(ctx context.Context) (MemoryHotplugStatus, error) {
	var st MemoryHotplugStatus
	err := c.do(ctx, http.MethodGet, "/hotplug/memory", nil, &st)
	return st, err
}

// SetMemoryHotplug asks the guest to plug sizeMiB of the virtio-mem region on top of
// its boot memory.
func (c *Client) SetMemoryHotplug(ctx context.Context, sizeMiB int64) error {
	return c.do(ctx, http.MethodPatch, "/hotplug/memory", map[string]int64{"requested_size_mib": sizeMiB}, nil)
}

// Snapshot types for SnapshotCreate.
const (
	SnapshotFull = "Full"
	SnapshotDiff = "Diff"
)

// SnapshotCreate is PUT /snapshot/create. Paths are as firecracker sees them.
type SnapshotCreate struct {
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
	SnapshotType string `json:"snapshot_type,omitempty"`
}

// CreateSnapshot writes the VM state and guest memory to files. The VM must be paused.
func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	return c.doWith(ctx, c.slow, http.MethodPut, "/snapshot/create", s, nil)
}

// MemoryBackend tells a snapshot load where the guest memory comes from. A File backend
// maps the file private, so VMs loaded from one file share its unchanged pages.
type MemoryBackend struct {
	BackendType string `json:"backend_type"` // "File" or "Uffd"
	BackendPath string `json:"backend_path"`
}

// NetworkOverride points a restored network interface at another tap.
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// VsockOverride points a restored vsock device at another unix socket.
type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

// SnapshotLoad is PUT /snapshot/load. Paths are as firecracker sees them.
type SnapshotLoad struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       MemoryBackend     `json:"mem_backend"`
	ResumeVM         bool              `json:"resume_vm,omitempty"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *VsockOverride    `json:"vsock_override,omitempty"`
	// ClockRealtime moves the guest clock forward by the time since the snapshot (x86_64 only).
	ClockRealtime bool `json:"clock_realtime,omitempty"`
}

// LoadSnapshot restores a VM from a snapshot. Firecracker only accepts it on a fresh
// process started without a config file.
func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	return c.doWith(ctx, c.slow, http.MethodPut, "/snapshot/load", s, nil)
}
