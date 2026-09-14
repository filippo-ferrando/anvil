// Package docker implements instance.Backend for containers running on
// Docker, talking to the Docker Engine API over its unix socket.
package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultSocket is where dockerd listens by default on Linux.
	DefaultSocket = "/var/run/docker.sock"

	// apiVersion is the Docker Engine API version this client targets.
	apiVersion = "v1.41"
)

// Client is a minimal Docker Engine API client over a unix socket.
type Client struct {
	http   *http.Client
	socket string
}

func NewClient(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	return &Client{
		socket: socket,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

func (c *Client) url(path string) string {
	return "http://unix/" + apiVersion + path
}

// do sends a request with an optional JSON body and returns the raw
// response. Callers must check StatusCode and close Body.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("docker: encoding request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), reader)
	if err != nil {
		return nil, fmt.Errorf("docker: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %s %s: %w (is dockerd running and reachable at %s?)", method, path, err, c.socket)
	}
	return resp, nil
}

// dockerError is the JSON shape Docker's API returns on a non-2xx response.
type dockerError struct {
	Message string `json:"message"`
}

// expectStatus reads and discards the body, returning an error unless the
// status is one of want.
func expectStatus(resp *http.Response, want ...int) error {
	defer resp.Body.Close()
	for _, w := range want {
		if resp.StatusCode == w {
			return nil
		}
	}
	return statusError(resp)
}

// decodeJSON checks the status is want, then decodes the JSON body into out.
func decodeJSON(resp *http.Response, out any, want int) error {
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return statusError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: decoding response: %w", err)
	}
	return nil
}

func statusError(resp *http.Response) error {
	var derr dockerError
	_ = json.NewDecoder(resp.Body).Decode(&derr)
	if derr.Message != "" {
		return fmt.Errorf("docker: %s: %s", resp.Status, derr.Message)
	}
	return fmt.Errorf("docker: unexpected status %s", resp.Status)
}

// --- container create ---

type portBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"`
}

type hostConfig struct {
	Binds        []string                 `json:"Binds,omitempty"`
	PortBindings map[string][]portBinding `json:"PortBindings,omitempty"`
	NetworkMode  string                   `json:"NetworkMode,omitempty"`
	// ExtraHosts holds static /etc/hosts entries, each formatted "host:ip".
	ExtraHosts []string `json:"ExtraHosts,omitempty"`
}

type endpointSettings struct {
	Aliases []string `json:"Aliases,omitempty"`
}

type networkingConfig struct {
	EndpointsConfig map[string]endpointSettings `json:"EndpointsConfig,omitempty"`
}

type createContainerRequest struct {
	Image            string              `json:"Image"`
	Env              []string            `json:"Env,omitempty"`
	Entrypoint       []string            `json:"Entrypoint,omitempty"`
	Cmd              []string            `json:"Cmd,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig       hostConfig          `json:"HostConfig"`
	NetworkingConfig *networkingConfig   `json:"NetworkingConfig,omitempty"`
}

type createContainerResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// CreateContainerParams holds the parameters needed to create a container.
type CreateContainerParams struct {
	Name        string
	Image       string
	Env         map[string]string
	Entrypoint  []string
	Cmd         []string
	Volumes     []VolumeMount
	Ports       []PortMapping
	NetworkMode string

	// NetworkAlias, if set, is an additional DNS name for this container
	// on its network. ExtraHosts adds static "host:ip" entries for peers.
	NetworkAlias string
	ExtraHosts   map[string]string
}

type VolumeMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

type PortMapping struct {
	HostPort  int
	GuestPort int
	Protocol  string // "tcp" | "udp", defaults to "tcp" if empty
}

// ImageExists reports whether ref is already present in Docker's local
// image store.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusError(resp)
	}
}

// splitImageRef splits ref into a repository and tag/digest, defaulting the
// tag to "latest" when ref has none.
func splitImageRef(ref string) (repo, tag string) {
	if idx := strings.Index(ref, "@"); idx != -1 {
		return ref[:idx], ref[idx+1:]
	}
	rest := ref
	prefixLen := 0
	if slash := strings.LastIndex(ref, "/"); slash != -1 {
		prefixLen = slash + 1
		rest = ref[prefixLen:]
	}
	if idx := strings.Index(rest, ":"); idx != -1 {
		return ref[:prefixLen+idx], rest[idx+1:]
	}
	return ref, "latest"
}

// pullProgressInterval throttles repeated progress updates for a layer
// whose status hasn't changed.
const pullProgressInterval = 500 * time.Millisecond

// PullImage pulls ref from its registry, same as `docker pull`. onProgress,
// if non-nil, receives a status line per layer as it changes.
func (c *Client) PullImage(ctx context.Context, ref string, onProgress func(status string)) error {
	repo, tag := splitImageRef(ref)
	path := "/images/create?fromImage=" + url.QueryEscape(repo) + "&tag=" + url.QueryEscape(tag)
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: pulling %s: %w", ref, statusError(resp))
	}

	type layerState struct {
		status   string
		lastSent time.Time
	}
	layers := make(map[string]*layerState)

	dec := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Status   string `json:"status"`
			ID       string `json:"id,omitempty"`
			Progress string `json:"progress,omitempty"`
			Error    string `json:"error"`
		}
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("docker: reading pull progress for %s: %w", ref, err)
		}
		if line.Error != "" {
			return fmt.Errorf("docker: pulling %s: %s", ref, line.Error)
		}
		if onProgress == nil || line.Status == "" {
			continue
		}

		text := func() string {
			if line.Progress != "" {
				return line.Status + " " + line.Progress
			}
			return line.Status
		}()
		if line.ID == "" {
			// An image-level line, not tied to a specific layer.
			onProgress(text)
			continue
		}

		st, ok := layers[line.ID]
		if !ok {
			st = &layerState{}
			layers[line.ID] = st
		}
		statusChanged := st.status != line.Status
		st.status = line.Status
		if statusChanged || time.Since(st.lastSent) >= pullProgressInterval {
			onProgress(line.ID + ": " + text)
			st.lastSent = time.Now()
		}
	}
}

// CreateContainer creates (but does not start) a container, returning its
// engine-assigned ID.
func (c *Client) CreateContainer(ctx context.Context, p CreateContainerParams) (string, error) {
	req := createContainerRequest{
		Image:      p.Image,
		Entrypoint: p.Entrypoint,
		Cmd:        p.Cmd,
		HostConfig: hostConfig{NetworkMode: p.NetworkMode},
	}
	for k, v := range p.Env {
		req.Env = append(req.Env, fmt.Sprintf("%s=%s", k, v))
	}
	for _, v := range p.Volumes {
		bind := v.HostPath + ":" + v.ContainerPath
		if v.ReadOnly {
			bind += ":ro"
		}
		req.HostConfig.Binds = append(req.HostConfig.Binds, bind)
	}
	if len(p.Ports) > 0 {
		req.ExposedPorts = make(map[string]struct{})
		req.HostConfig.PortBindings = make(map[string][]portBinding)
		for _, port := range p.Ports {
			proto := port.Protocol
			if proto == "" {
				proto = "tcp"
			}
			key := fmt.Sprintf("%d/%s", port.GuestPort, proto)
			req.ExposedPorts[key] = struct{}{}
			req.HostConfig.PortBindings[key] = []portBinding{{HostPort: fmt.Sprintf("%d", port.HostPort)}}
		}
	}
	for name, ip := range p.ExtraHosts {
		req.HostConfig.ExtraHosts = append(req.HostConfig.ExtraHosts, name+":"+ip)
	}
	if p.NetworkAlias != "" && p.NetworkMode != "" {
		req.NetworkingConfig = &networkingConfig{
			EndpointsConfig: map[string]endpointSettings{
				p.NetworkMode: {Aliases: []string{p.NetworkAlias}},
			},
		}
	}

	path := "/containers/create"
	if p.Name != "" {
		path += "?name=" + url.QueryEscape(p.Name)
	}
	resp, err := c.do(ctx, http.MethodPost, path, req)
	if err != nil {
		return "", err
	}
	var out createContainerResponse
	if err := decodeJSON(resp, &out, http.StatusCreated); err != nil {
		return "", fmt.Errorf("docker: creating container: %w", err)
	}
	return out.ID, nil
}

// StartContainer starts an already-created container. Idempotent: starting
// an already-running container is treated as success (Docker returns 304).
func (c *Client) StartContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, http.StatusNotModified); err != nil {
		return fmt.Errorf("docker: starting container %s: %w", id, err)
	}
	return nil
}

// StopContainer stops a running container, giving it timeoutSeconds to
// exit gracefully before Docker forces it to stop.
func (c *Client) StopContainer(ctx context.Context, id string, timeoutSeconds int) error {
	path := fmt.Sprintf("/containers/%s/stop?t=%d", id, timeoutSeconds)
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, http.StatusNotModified); err != nil {
		return fmt.Errorf("docker: stopping container %s: %w", id, err)
	}
	return nil
}

// RemoveContainer deletes a container. force also removes a running one
// (stopping it first), matching `docker rm -f`.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	path := "/containers/" + id
	if force {
		path += "?force=true"
	}
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	// A container that's already gone isn't treated as a failure.
	if err := expectStatus(resp, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("docker: removing container %s: %w", id, err)
	}
	return nil
}

// ContainerState is the subset of `docker inspect`'s State object this
// package needs.
type ContainerState struct {
	Status    string `json:"Status"` // "created" | "running" | "paused" | "restarting" | "removing" | "exited" | "dead"
	Running   bool   `json:"Running"`
	StartedAt string `json:"StartedAt"` // RFC3339Nano; zero-value time string when never started
}

type networkSettings struct {
	IPAddress string `json:"IPAddress"` // legacy top-level field, populated for the default bridge network
	Networks  map[string]struct {
		IPAddress string `json:"IPAddress"`
	} `json:"Networks"`
}

type inspectResponse struct {
	State           ContainerState  `json:"State"`
	NetworkSettings networkSettings `json:"NetworkSettings"`
}

// InspectState returns id's current state, queried fresh from Docker.
func (c *Client) InspectState(ctx context.Context, id string) (ContainerState, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil)
	if err != nil {
		return ContainerState{}, err
	}
	var out inspectResponse
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return ContainerState{}, fmt.Errorf("docker: inspecting container %s: %w", id, err)
	}
	return out.State, nil
}

// Inspection is id's live status, address, and start time.
type Inspection struct {
	Running   bool
	StartedAt time.Time
	Address   string
}

// Inspect returns id's current status, best-known address, and start time.
func (c *Client) Inspect(ctx context.Context, id string) (Inspection, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil)
	if err != nil {
		return Inspection{}, err
	}
	var out inspectResponse
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return Inspection{}, fmt.Errorf("docker: inspecting container %s: %w", id, err)
	}
	addr := out.NetworkSettings.IPAddress
	if addr == "" {
		for _, n := range out.NetworkSettings.Networks {
			if n.IPAddress != "" {
				addr = n.IPAddress
				break
			}
		}
	}
	startedAt, _ := time.Parse(time.RFC3339Nano, out.State.StartedAt)
	return Inspection{Running: out.State.Running, StartedAt: startedAt, Address: addr}, nil
}

// StatsSnapshot is id's cumulative resource-usage counters at one instant —
// two snapshots taken a short time apart let a caller compute live rates.
type StatsSnapshot struct {
	At time.Time

	CPUTotalUsageNanos uint64
	CPUSystemNanos     uint64
	OnlineCPUs         uint32

	MemUsedBytes  int64
	MemLimitBytes int64

	NetRxBytes uint64
	NetTxBytes uint64

	BlkReadBytes  uint64
	BlkWriteBytes uint64
}

type dockerStatsResponse struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

// Stats returns id's current resource-usage counters, a single HTTP
// round trip (Docker's own "stream=false" mode still populates a valid
// cpu_stats snapshot, just not the historical precpu_stats pairing this
// package uses for a rate — see docker/backend Stats callers, which take
// two of these a short time apart instead).
func (c *Client) Stats(ctx context.Context, id string) (StatsSnapshot, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/stats?stream=false", nil)
	if err != nil {
		return StatsSnapshot{}, err
	}
	var out dockerStatsResponse
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return StatsSnapshot{}, fmt.Errorf("docker: reading stats for container %s: %w", id, err)
	}

	var rx, tx uint64
	for _, n := range out.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	var read, write uint64
	for _, e := range out.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			read += e.Value
		case "write":
			write += e.Value
		}
	}
	ncpus := out.CPUStats.OnlineCPUs
	if ncpus == 0 {
		ncpus = 1
	}

	return StatsSnapshot{
		At:                 time.Now(),
		CPUTotalUsageNanos: out.CPUStats.CPUUsage.TotalUsage,
		CPUSystemNanos:     out.CPUStats.SystemCPUUsage,
		OnlineCPUs:         ncpus,
		MemUsedBytes:       int64(out.MemoryStats.Usage),
		MemLimitBytes:      int64(out.MemoryStats.Limit),
		NetRxBytes:         rx,
		NetTxBytes:         tx,
		BlkReadBytes:       read,
		BlkWriteBytes:      write,
	}, nil
}

// ContainerNetworkAddress returns id's assigned IP address on networkName.
func (c *Client) ContainerNetworkAddress(ctx context.Context, id, networkName string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil)
	if err != nil {
		return "", err
	}
	var out inspectResponse
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return "", fmt.Errorf("docker: inspecting container %s: %w", id, err)
	}
	net, ok := out.NetworkSettings.Networks[networkName]
	if !ok || net.IPAddress == "" {
		return "", fmt.Errorf("docker: container %s has no address on network %s", id, networkName)
	}
	return net.IPAddress, nil
}

// Logs streams id's stdout+stderr to send, one demuxed chunk at a time.
// tailLines of 0 means from the beginning.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tailLines int, send func([]byte) error) error {
	tail := "all"
	if tailLines > 0 {
		tail = fmt.Sprintf("%d", tailLines)
	}
	path := fmt.Sprintf("/containers/%s/logs?stdout=true&stderr=true&follow=%t&tail=%s", id, follow, tail)
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return demuxLogs(resp.Body, send)
}

// demuxLogs strips Docker's log stream framing: each chunk is prefixed
// with an 8-byte header giving its stream type and size, then the payload.
func demuxLogs(r io.Reader, send func([]byte) error) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("docker: reading log stream header: %w", err)
		}
		size := binary.BigEndian.Uint32(header[4:8])
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return fmt.Errorf("docker: reading log stream payload: %w", err)
		}
		if err := send(payload); err != nil {
			return err
		}
	}
}
