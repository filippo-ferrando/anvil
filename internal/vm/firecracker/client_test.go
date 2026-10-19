//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorded struct {
	method, path string
	body         map[string]any
}

// fakeAPI serves handler on a unix socket the way firecracker serves its API.
func fakeAPI(t *testing.T, handler func(w http.ResponseWriter, r recorded)) (*Client, *[]recorded) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls []recorded
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			_ = json.Unmarshal(data, &rec.body)
		}
		calls = append(calls, rec)
		handler(w, rec)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return NewClient(sock), &calls
}

func TestClientCalls(t *testing.T) {
	c, calls := fakeAPI(t, func(w http.ResponseWriter, r recorded) {
		switch r.method + " " + r.path {
		case "GET /":
			_, _ = w.Write([]byte(`{"id":"anvil","state":"Running","vmm_version":"1.17.0","app_name":"Firecracker"}`))
		case "GET /hotplug/memory":
			_, _ = w.Write([]byte(`{"total_size_mib":4096,"plugged_size_mib":512,"requested_size_mib":512}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()

	info, err := c.Info(ctx)
	if err != nil || info.State != StateRunning || info.VMMVersion != "1.17.0" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	mem, err := c.MemoryHotplug(ctx)
	if err != nil || mem.TotalSizeMiB != 4096 || mem.PluggedSizeMiB != 512 {
		t.Fatalf("MemoryHotplug = %+v, %v", mem, err)
	}
	for _, call := range []func(context.Context) error{
		c.SendCtrlAltDel, c.Pause, c.Resume,
		func(ctx context.Context) error { return c.UpdateDrive(ctx, "rootfs", "/d/disk.raw") },
		func(ctx context.Context) error { return c.SetMemoryHotplug(ctx, 1024) },
	} {
		if err := call(ctx); err != nil {
			t.Fatal(err)
		}
	}

	want := []struct {
		method, path, key string
		value             any
	}{
		{"GET", "/", "", nil},
		{"GET", "/hotplug/memory", "", nil},
		{"PUT", "/actions", "action_type", "SendCtrlAltDel"},
		{"PATCH", "/vm", "state", "Paused"},
		{"PATCH", "/vm", "state", "Resumed"},
		{"PATCH", "/drives/rootfs", "path_on_host", "/d/disk.raw"},
		{"PATCH", "/hotplug/memory", "requested_size_mib", float64(1024)},
	}
	if len(*calls) != len(want) {
		t.Fatalf("got %d calls, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		got := (*calls)[i]
		if got.method != w.method || got.path != w.path || (w.key != "" && got.body[w.key] != w.value) {
			t.Errorf("call %d = %s %s %v, want %s %s %s=%v", i, got.method, got.path, got.body, w.method, w.path, w.key, w.value)
		}
	}
}

func TestClientFault(t *testing.T) {
	c, _ := fakeAPI(t, func(w http.ResponseWriter, _ recorded) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"fault_message":"The requested operation is not supported in the current state."}`))
	})
	err := c.Pause(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Fault, "not supported") {
		t.Fatalf("err = %v, want an APIError with the fault message", err)
	}
}

func TestClientNoSocket(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("expected an error without a socket")
	}
}

func TestConfigWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Config{
		BootSource:    BootSource{KernelImagePath: "/k", BootArgs: "console=ttyS0"},
		MachineConfig: MachineConfig{VCPUCount: 2, MemSizeMiB: 1024},
		Drives:        []Drive{{DriveID: "rootfs", PathOnHost: "/d", IsRootDevice: true, PartUUID: "abc"}},
		Vsock:         &Vsock{GuestCID: GuestCID, UDSPath: "/v.sock"},
		Entropy:       &struct{}{},
	}
	if err := cfg.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"boot-source", "machine-config", "drives", "vsock", "entropy"} {
		if _, ok := got[key]; !ok {
			t.Errorf("config is missing %q: %s", key, data)
		}
	}
	for _, key := range []string{"network-interfaces", "memory-hotplug"} {
		if _, ok := got[key]; ok {
			t.Errorf("config has unset %q: %s", key, data)
		}
	}
	if d := got["drives"].([]any)[0].(map[string]any); d["partuuid"] != "abc" || d["is_read_only"] != false {
		t.Errorf("drive = %v", d)
	}

	if err := (Config{}).WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `"drives": []`) {
		t.Errorf("a config without drives must list none, not null: %s", data)
	}
}
