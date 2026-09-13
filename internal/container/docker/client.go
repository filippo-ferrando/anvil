// Package docker implements instance.Backend for containers running on
// Docker (instance.ContainerEngineDocker) — see backend.go. This talks to
// the Docker Engine API directly over its unix socket with a small,
// hand-rolled HTTP client (client.go), rather than depending on
// github.com/docker/docker/client: that SDK's package layout has moved
// around across versions in ways there was no way to verify against here
// (no network access to fetch and inspect it, no way to run a real
// dockerd in this sandbox either — checked, needs root or rootless
// tooling neither of which is available here). The wire-level REST API
// itself is far more stable and well documented than the Go SDK's
// internal structure, so hand-rolling against it is the same tradeoff
// already made for QMP (internal/vm/qemu) and is fully stdlib, no new
// dependency to get wrong.
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

	// apiVersion is deliberately conservative (not the newest the daemon
	// might support): Docker's API negotiation is fine with a client
	// requesting an older, still-supported version prefix, and pinning
	// one here means this doesn't silently start depending on a field
	// only a very recent daemon has.
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
// response — callers are responsible for checking StatusCode and closing
// Body (via decodeJSON, expectStatus, or directly).
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
	// ExtraHosts is Docker's own name for a static /etc/hosts entry
	// (`docker run --add-host host:ip`), each formatted "host:ip" — this
	// is how a container resolves an intent's VM members, which aren't
	// Docker-managed and so aren't visible to Docker's embedded DNS. See
	// NetworkAlias on endpointSettings for the reverse direction
	// (container peers resolving this one).
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

// CreateContainerParams mirrors the subset of instance.ContainerSpec this
// package actually needs — kept separate from that type so this package
// doesn't import internal/instance (internal/container/docker_backend.go
// does the translation, same pattern as internal/vm/qemu.Config staying
// decoupled from VMSpec).
type CreateContainerParams struct {
	Name        string
	Image       string
	Env         map[string]string
	Entrypoint  []string
	Cmd         []string
	Volumes     []VolumeMount
	Ports       []PortMapping
	NetworkMode string

	// NetworkAlias, if set, is an additional network-scoped DNS name
	// Docker's embedded DNS resolves to this container (on top of its own
	// container name) — see internal/intent.Manager, which sets this to
	// an intent member's role. ExtraHosts adds static "host:ip" entries
	// (Docker's --add-host equivalent) for peers Docker's own DNS can't
	// resolve (VM members).
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
// image store. Plain `POST /containers/create` does NOT auto-pull a
// missing image the way the `docker run` CLI appears to (it 404s instead),
// so callers need this (plus PullImage below) to get that same convenience.
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

// splitImageRef splits ref into the repository and tag/digest Docker's pull
// endpoint wants as separate query parameters. A bare digest reference
// (repo@sha256:...) keeps the digest as-is; otherwise the tag is whatever
// follows the last ":" after the final "/" (so a registry host with its
// own port, like "host:5000/name", isn't mistaken for a tag separator), or
// "latest" if there's no tag at all — the same default Docker itself uses.
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

// pullProgressInterval bounds how often the same layer's progress gets
// forwarded to onProgress while its status isn't otherwise changing (e.g.
// while it's sitting in "Downloading" with just the byte count moving) —
// see PullImage's doc comment for why this exists at all.
const pullProgressInterval = 500 * time.Millisecond

// PullImage pulls ref from its registry, same as `docker pull`. onProgress,
// if non-nil, is called with a human-readable line per layer as its status
// changes (e.g. "a3ed95c: Pulling fs layer" -> "a3ed95c: Downloading" ->
// "a3ed95c: Download complete"), throttled while a layer sits in the same
// status so a long download doesn't flood the caller with near-identical
// lines — but never silent for longer than pullProgressInterval either,
// which is the actual point: telling a slow pull apart from a stuck one.
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
			// An image-level line, not tied to a specific layer (e.g.
			// "Pulling from library/nginx", the final "Status: Downloaded
			// newer image for ...") — always forwarded, there's no
			// per-layer spam risk here.
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
		path += "?name=" + p.Name
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
// exit gracefully (SIGTERM) before Docker escalates to SIGKILL itself —
// Docker's own stop endpoint already implements exactly the escalation
// internal/vm/qemu.Process.Stop hand-rolls for QEMU, so there's no need to
// reimplement that here.
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
	// A container that's already gone isn't a failure for our purposes —
	// Delete should be idempotent, same as the VM backend's Delete.
	if err := expectStatus(resp, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("docker: removing container %s: %w", id, err)
	}
	return nil
}

// ContainerState is the subset of `docker inspect`'s State object this
// package needs.
type ContainerState struct {
	Status  string `json:"Status"` // "created" | "running" | "paused" | "restarting" | "removing" | "exited" | "dead"
	Running bool   `json:"Running"`
}

type networkSettings struct {
	Networks map[string]struct {
		IPAddress string `json:"IPAddress"`
	} `json:"Networks"`
}

type inspectResponse struct {
	State           ContainerState  `json:"State"`
	NetworkSettings networkSettings `json:"NetworkSettings"`
}

// InspectState returns id's current state. Unlike the VM backend, there's
// no in-process "is it running" tracking to lose on a daemon restart —
// Docker itself is the source of truth, queried fresh every time.
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

// ContainerNetworkAddress returns id's assigned IP address on networkName
// — used by internal/intent.Manager right after creating an intent
// member container, so a later-launched VM member's ExtraHosts can
// resolve it (see instance.VMSpec.ExtraHosts). Docker assigns this
// address itself (via its own IPAM), it isn't something anvil picks.
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

// Logs streams id's stdout+stderr to send, one demuxed chunk at a time —
// see demuxLogs for why this needs demultiplexing at all. tailLines of 0
// means "from the beginning" (Docker's own convention: an empty/"all"
// tail value, sent as "all").
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

// demuxLogs strips Docker's log stream framing: when a container wasn't
// created with a TTY (anvil never allocates one — see backend.go), each
// chunk of stdout/stderr is prefixed with an 8-byte header
// [stream-type(1), 0, 0, 0, size(4 bytes, big-endian)] followed by that
// many bytes of payload. This has been Docker's stable, documented log
// stream format for a long time.
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
