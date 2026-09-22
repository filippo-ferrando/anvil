package docker

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer starts handler listening on a unix socket in t.TempDir()
// and returns a Client pointed at it.
func newTestServer(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on unix socket: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return NewClient(socket)
}

func TestCreateContainer(t *testing.T) {
	var gotPath string
	var gotBody createContainerRequest
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createContainerResponse{ID: "abc123"})
	}))

	id, err := c.CreateContainer(t.Context(), CreateContainerParams{
		Name:  "test1",
		Image: "nginx:latest",
		Env:   map[string]string{"FOO": "bar"},
		Volumes: []VolumeMount{
			{HostPath: "/host", ContainerPath: "/container", ReadOnly: true},
		},
		Ports: []PortMapping{
			{HostPort: 8080, GuestPort: 80, Protocol: "tcp"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if id != "abc123" {
		t.Errorf("expected id abc123, got %s", id)
	}
	if !strings.HasPrefix(gotPath, "/"+apiVersion+"/containers/create") {
		t.Errorf("unexpected request path: %s", gotPath)
	}
	if !strings.Contains(gotPath, "name=test1") {
		t.Errorf("expected name query param, got path %s", gotPath)
	}
	if gotBody.Image != "nginx:latest" {
		t.Errorf("expected image nginx:latest, got %s", gotBody.Image)
	}
	if len(gotBody.Env) != 1 || gotBody.Env[0] != "FOO=bar" {
		t.Errorf("expected Env [FOO=bar], got %v", gotBody.Env)
	}
	if len(gotBody.HostConfig.Binds) != 1 || gotBody.HostConfig.Binds[0] != "/host:/container:ro" {
		t.Errorf("expected a ro bind, got %v", gotBody.HostConfig.Binds)
	}
	if _, ok := gotBody.ExposedPorts["80/tcp"]; !ok {
		t.Errorf("expected ExposedPorts to contain 80/tcp, got %v", gotBody.ExposedPorts)
	}
	bindings := gotBody.HostConfig.PortBindings["80/tcp"]
	if len(bindings) != 1 || bindings[0].HostPort != "8080" {
		t.Errorf("expected port binding 8080, got %v", bindings)
	}
}

func TestCreateContainerNetworkAliasAndExtraHosts(t *testing.T) {
	var gotBody createContainerRequest
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createContainerResponse{ID: "abc123"})
	}))

	_, err := c.CreateContainer(t.Context(), CreateContainerParams{
		Image:        "nginx:latest",
		NetworkMode:  "anvil-myapp",
		NetworkAlias: "web",
		ExtraHosts:   map[string]string{"db": "10.55.201.3"},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(gotBody.HostConfig.ExtraHosts) != 1 || gotBody.HostConfig.ExtraHosts[0] != "db:10.55.201.3" {
		t.Errorf("expected ExtraHosts [db:10.55.201.3], got %v", gotBody.HostConfig.ExtraHosts)
	}
	if gotBody.NetworkingConfig == nil {
		t.Fatal("expected a NetworkingConfig to be set")
	}
	ep, ok := gotBody.NetworkingConfig.EndpointsConfig["anvil-myapp"]
	if !ok || len(ep.Aliases) != 1 || ep.Aliases[0] != "web" {
		t.Errorf("expected network alias \"web\" on anvil-myapp, got %v", gotBody.NetworkingConfig.EndpointsConfig)
	}
}

func TestCreateContainerDNS(t *testing.T) {
	var gotBody createContainerRequest
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createContainerResponse{ID: "abc123"})
	}))

	_, err := c.CreateContainer(t.Context(), CreateContainerParams{
		Image:       "nginx:latest",
		NetworkMode: "anvil-myapp",
		DNSServers:  []string{"10.55.201.1"},
		DNSSearch:   []string{"myapp.anvil"},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(gotBody.HostConfig.Dns) != 1 || gotBody.HostConfig.Dns[0] != "10.55.201.1" {
		t.Errorf("expected Dns [10.55.201.1], got %v", gotBody.HostConfig.Dns)
	}
	if len(gotBody.HostConfig.DnsSearch) != 1 || gotBody.HostConfig.DnsSearch[0] != "myapp.anvil" {
		t.Errorf("expected DnsSearch [myapp.anvil], got %v", gotBody.HostConfig.DnsSearch)
	}
}

func TestCreateContainerNoNetworkAliasWithoutNetworkMode(t *testing.T) {
	// NetworkingConfig should stay nil when NetworkMode isn't set.
	var gotBody createContainerRequest
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createContainerResponse{ID: "abc123"})
	}))

	_, err := c.CreateContainer(t.Context(), CreateContainerParams{
		Image:        "nginx:latest",
		NetworkAlias: "web",
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if gotBody.NetworkingConfig != nil {
		t.Errorf("expected no NetworkingConfig without a NetworkMode, got %v", gotBody.NetworkingConfig)
	}
}

func TestContainerNetworkAddress(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"NetworkSettings": map[string]any{
				"Networks": map[string]any{
					"anvil-myapp": map[string]any{"IPAddress": "10.55.201.130"},
				},
			},
		})
	}))

	ip, err := c.ContainerNetworkAddress(t.Context(), "abc123", "anvil-myapp")
	if err != nil {
		t.Fatalf("ContainerNetworkAddress: %v", err)
	}
	if ip != "10.55.201.130" {
		t.Errorf("expected 10.55.201.130, got %s", ip)
	}
}

func TestContainerNetworkAddressMissingNetwork(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"NetworkSettings": map[string]any{"Networks": map[string]any{}}})
	}))

	if _, err := c.ContainerNetworkAddress(t.Context(), "abc123", "anvil-myapp"); err == nil {
		t.Error("expected an error when the container has no address on that network")
	}
}

func TestCreateContainerErrorPropagates(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(dockerError{Message: "no such image"})
	}))

	_, err := c.CreateContainer(t.Context(), CreateContainerParams{Image: "does-not-exist"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("expected the Docker error message to surface, got: %v", err)
	}
}

func TestStartStopRemoveContainer(t *testing.T) {
	var calls []string
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))

	if err := c.StartContainer(t.Context(), "abc123"); err != nil {
		t.Errorf("StartContainer: %v", err)
	}
	if err := c.StopContainer(t.Context(), "abc123", 15); err != nil {
		t.Errorf("StopContainer: %v", err)
	}
	if err := c.RemoveContainer(t.Context(), "abc123", true); err != nil {
		t.Errorf("RemoveContainer: %v", err)
	}

	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "/containers/abc123/start") {
		t.Errorf("expected a start call, got: %s", joined)
	}
	if !strings.Contains(joined, "/containers/abc123/stop?t=15") {
		t.Errorf("expected stop with t=15, got: %s", joined)
	}
	if !strings.Contains(joined, "DELETE /"+apiVersion+"/containers/abc123?force=true") {
		t.Errorf("expected a forced delete, got: %s", joined)
	}
}

func TestRemoveContainerAlreadyGoneIsNotAnError(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if err := c.RemoveContainer(t.Context(), "gone", false); err != nil {
		t.Errorf("expected removing an already-gone container to succeed, got: %v", err)
	}
}

func TestInspectState(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(inspectResponse{State: ContainerState{Status: "running", Running: true}})
	}))
	state, err := c.InspectState(t.Context(), "abc123")
	if err != nil {
		t.Fatalf("InspectState: %v", err)
	}
	if !state.Running || state.Status != "running" {
		t.Errorf("unexpected state: %+v", state)
	}
}

func TestInspectReturnsStartedAtAndAddress(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"State": map[string]any{
				"Running":   true,
				"StartedAt": "2024-01-02T03:04:05.123456789Z",
			},
			"NetworkSettings": map[string]any{
				"IPAddress": "172.17.0.5",
			},
		})
	}))
	insp, err := c.Inspect(t.Context(), "abc123")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !insp.Running {
		t.Error("expected Running to be true")
	}
	if insp.Address != "172.17.0.5" {
		t.Errorf("expected the top-level IPAddress, got %q", insp.Address)
	}
	if insp.StartedAt.IsZero() || insp.StartedAt.Year() != 2024 {
		t.Errorf("expected StartedAt to parse to 2024, got %v", insp.StartedAt)
	}
}

func TestInspectFallsBackToPerNetworkAddress(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"State": map[string]any{"Running": true},
			"NetworkSettings": map[string]any{
				"IPAddress": "", // empty: attached only to a custom (non-default) network
				"Networks": map[string]any{
					"anvil-myapp": map[string]any{"IPAddress": "10.55.201.4"},
				},
			},
		})
	}))
	insp, err := c.Inspect(t.Context(), "abc123")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if insp.Address != "10.55.201.4" {
		t.Errorf("expected the per-network address as a fallback, got %q", insp.Address)
	}
}

func TestStatsComputesCumulativeCounters(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1.41/containers/abc123/stats"; got != want {
			t.Errorf("expected path %q, got %q", want, got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": 500},
				"system_cpu_usage": 10000,
				"online_cpus":      2,
			},
			"memory_stats": map[string]any{"usage": 1024, "limit": 2048},
			"networks": map[string]any{
				"eth0": map[string]any{"rx_bytes": 100, "tx_bytes": 50},
			},
			"blkio_stats": map[string]any{
				"io_service_bytes_recursive": []map[string]any{
					{"op": "Read", "value": 300},
					{"op": "Write", "value": 400},
					{"op": "Read", "value": 20},
				},
			},
		})
	}))
	snap, err := c.Stats(t.Context(), "abc123")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if snap.CPUTotalUsageNanos != 500 || snap.CPUSystemNanos != 10000 || snap.OnlineCPUs != 2 {
		t.Errorf("unexpected CPU counters: %+v", snap)
	}
	if snap.MemUsedBytes != 1024 || snap.MemLimitBytes != 2048 {
		t.Errorf("unexpected memory counters: %+v", snap)
	}
	if snap.NetRxBytes != 100 || snap.NetTxBytes != 50 {
		t.Errorf("unexpected network counters: %+v", snap)
	}
	if snap.BlkReadBytes != 320 || snap.BlkWriteBytes != 400 {
		t.Errorf("unexpected blkio counters (Read entries should sum): %+v", snap)
	}
}

func TestListNetworks(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1.41/networks"; got != want {
			t.Errorf("expected path %q, got %q", want, got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Name": "anvil-01ABC"},
			{"Name": "bridge"},
		})
	}))
	names, err := c.ListNetworks(t.Context())
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}
	if len(names) != 2 || names[0] != "anvil-01ABC" || names[1] != "bridge" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestListImages(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1.41/images/json"; got != want {
			t.Errorf("expected path %q, got %q", want, got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "sha256:abc", "RepoTags": []string{"nginx:alpine"}, "Size": 1000},
			{"Id": "sha256:def", "RepoTags": []string{"<none>:<none>"}, "Size": 2000},
		})
	}))
	images, err := c.ListImages(t.Context())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}
	if images[0].ID != "sha256:abc" || len(images[0].RepoTags) != 1 || images[0].RepoTags[0] != "nginx:alpine" {
		t.Errorf("unexpected first image: %+v", images[0])
	}
	if images[1].RepoTags != nil {
		t.Errorf("expected a <none>:<none> RepoTags to normalize to nil, got %v", images[1].RepoTags)
	}
}

func TestImagesInUseCountsByImageID(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.RawQuery, "all=true"; got != want {
			t.Errorf("expected query %q, got %q", want, got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ImageID": "sha256:abc"},
			{"ImageID": "sha256:abc"},
			{"ImageID": "sha256:def"},
		})
	}))
	counts, err := c.ImagesInUse(t.Context())
	if err != nil {
		t.Fatalf("ImagesInUse: %v", err)
	}
	if counts["sha256:abc"] != 2 || counts["sha256:def"] != 1 {
		t.Errorf("unexpected counts: %+v", counts)
	}
}

func TestRemoveImage(t *testing.T) {
	var gotPath, gotMethod string
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path+"?"+r.URL.RawQuery, r.Method
		w.WriteHeader(http.StatusOK)
	}))
	if err := c.RemoveImage(t.Context(), "sha256:abc", true); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if want := "/v1.41/images/sha256:abc?force=true"; gotPath != want {
		t.Errorf("expected path %q, got %q", want, gotPath)
	}
}

func TestRemoveImageAlreadyGoneIsNotAnError(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if err := c.RemoveImage(t.Context(), "gone", false); err != nil {
		t.Errorf("expected removing an already-gone image to succeed, got: %v", err)
	}
}

func TestLogsDemux(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFrame(w, 1, "hello stdout\n")
		writeFrame(w, 2, "hello stderr\n")
	}))

	var chunks []string
	err := c.Logs(t.Context(), "abc123", false, 0, func(b []byte) error {
		chunks = append(chunks, string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(chunks) != 2 || chunks[0] != "hello stdout\n" || chunks[1] != "hello stderr\n" {
		t.Errorf("expected two demuxed chunks, got %v", chunks)
	}
}

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		ref, wantRepo, wantTag string
	}{
		{"nginx", "nginx", "latest"},
		{"nginx:alpine", "nginx", "alpine"},
		{"nginx:latest", "nginx", "latest"},
		{"library/nginx", "library/nginx", "latest"},
		{"library/nginx:alpine", "library/nginx", "alpine"},
		{"myregistry:5000/nginx", "myregistry:5000/nginx", "latest"},
		{"myregistry:5000/nginx:alpine", "myregistry:5000/nginx", "alpine"},
		{"nginx@sha256:abc123", "nginx", "sha256:abc123"},
	}
	for _, tc := range cases {
		repo, tag := splitImageRef(tc.ref)
		if repo != tc.wantRepo || tag != tc.wantTag {
			t.Errorf("splitImageRef(%q) = (%q, %q), want (%q, %q)", tc.ref, repo, tag, tc.wantRepo, tc.wantTag)
		}
	}
}

func TestImageExists(t *testing.T) {
	var gotPath string
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "present") {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:abc"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	exists, err := c.ImageExists(t.Context(), "present:latest")
	if err != nil {
		t.Fatalf("ImageExists: %v", err)
	}
	if !exists {
		t.Error("expected present:latest to exist")
	}
	if !strings.Contains(gotPath, "/images/present:latest/json") {
		t.Errorf("unexpected request path: %s", gotPath)
	}

	exists, err = c.ImageExists(t.Context(), "missing:latest")
	if err != nil {
		t.Fatalf("ImageExists: %v", err)
	}
	if exists {
		t.Error("expected missing:latest to not exist")
	}
}

func TestPullImageStreamsProgressAndSucceeds(t *testing.T) {
	var gotPath string
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]string{"status": "Pulling from library/nginx"})
		_ = enc.Encode(map[string]string{"status": "Download complete"})
	}))

	var statuses []string
	err := c.PullImage(t.Context(), "nginx:alpine", func(s string) { statuses = append(statuses, s) })
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if len(statuses) != 2 {
		t.Errorf("expected 2 progress lines, got %v", statuses)
	}
	if !strings.Contains(gotPath, "/images/create?fromImage=nginx&tag=alpine") {
		t.Errorf("unexpected request path: %s", gotPath)
	}
}

func TestPullImageCollapsesRepeatedLayerStatusButKeepsTransitions(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		// Same layer and status repeated five times, then two transitions.
		_ = enc.Encode(map[string]string{"id": "abc123", "status": "Pulling fs layer"})
		for i := 0; i < 5; i++ {
			_ = enc.Encode(map[string]string{"id": "abc123", "status": "Downloading", "progress": "[=>] 1MB/5MB"})
		}
		_ = enc.Encode(map[string]string{"id": "abc123", "status": "Pull complete"})
	}))

	var statuses []string
	if err := c.PullImage(t.Context(), "nginx:alpine", func(s string) { statuses = append(statuses, s) }); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("expected 3 forwarded lines (2 transitions + first Downloading), got %d: %v", len(statuses), statuses)
	}
	if statuses[0] != "abc123: Pulling fs layer" {
		t.Errorf("unexpected first line: %q", statuses[0])
	}
	if !strings.HasPrefix(statuses[1], "abc123: Downloading") {
		t.Errorf("unexpected second line: %q", statuses[1])
	}
	if statuses[2] != "abc123: Pull complete" {
		t.Errorf("unexpected third line: %q", statuses[2])
	}
}

func TestPullImagePropagatesDaemonError(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such image or tag"})
	}))

	err := c.PullImage(t.Context(), "does-not-exist:latest", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no such image or tag") {
		t.Errorf("expected the daemon's error message to surface, got: %v", err)
	}
}

func TestCreateNetwork(t *testing.T) {
	var gotPath string
	var gotBody createNetworkRequest
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createNetworkResponse{ID: "net123"})
	}))

	id, err := c.CreateNetwork(t.Context(), NetworkCreateParams{
		Name:            "anvil-myapp",
		BridgeInterface: "anvil0abc123",
		Subnet:          "10.55.201.0/24",
		Gateway:         "10.55.201.1",
		IPRange:         "10.55.201.128/25",
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if id != "net123" {
		t.Errorf("expected id net123, got %s", id)
	}
	if !strings.HasSuffix(gotPath, "/networks/create") {
		t.Errorf("unexpected request path: %s", gotPath)
	}
	if gotBody.Driver != "bridge" {
		t.Errorf("expected bridge driver, got %s", gotBody.Driver)
	}
	if gotBody.Options["com.docker.network.bridge.name"] != "anvil0abc123" {
		t.Errorf("expected explicit bridge name option, got %v", gotBody.Options)
	}
	if len(gotBody.IPAM.Config) != 1 || gotBody.IPAM.Config[0].Subnet != "10.55.201.0/24" {
		t.Errorf("expected subnet in IPAM config, got %v", gotBody.IPAM)
	}
}

func TestNetworkExists(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "present") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	exists, err := c.NetworkExists(t.Context(), "present")
	if err != nil {
		t.Fatalf("NetworkExists: %v", err)
	}
	if !exists {
		t.Error("expected present to exist")
	}

	exists, err = c.NetworkExists(t.Context(), "missing")
	if err != nil {
		t.Fatalf("NetworkExists: %v", err)
	}
	if exists {
		t.Error("expected missing to not exist")
	}
}

func TestRemoveNetworkAlreadyGoneIsNotAnError(t *testing.T) {
	c := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if err := c.RemoveNetwork(t.Context(), "gone"); err != nil {
		t.Errorf("expected removing an already-gone network to succeed, got: %v", err)
	}
}

func writeFrame(w http.ResponseWriter, streamType byte, payload string) {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	w.Write(header)
	w.Write([]byte(payload))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
