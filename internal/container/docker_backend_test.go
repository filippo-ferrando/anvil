package container

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

type fakeContainer struct {
	name    string
	ports   []string
	running bool
}

// portDocker keeps containers in memory and refuses to start one publishing host port 8080.
type portDocker struct {
	mu         sync.Mutex
	next       int
	containers map[string]*fakeContainer
}

func (d *portDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // [version, kind, id, action]
	switch {
	case parts[1] == "images":
		w.WriteHeader(http.StatusOK)
	case parts[2] == "create":
		name := r.URL.Query().Get("name")
		for _, c := range d.containers {
			if c.name == name {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		var req struct {
			HostConfig struct {
				PortBindings map[string][]struct{ HostPort string }
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		c := &fakeContainer{name: name}
		for _, b := range req.HostConfig.PortBindings {
			c.ports = append(c.ports, b[0].HostPort)
		}
		d.next++
		id := fmt.Sprintf("c%d", d.next)
		d.containers[id] = c
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	default:
		c, ok := d.containers[parts[2]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodDelete:
			delete(d.containers, parts[2])
			w.WriteHeader(http.StatusNoContent)
		case parts[3] == "json":
			status := "exited"
			if c.running {
				status = "running"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Status": status}})
		case parts[3] == "stop":
			c.running = false
			w.WriteHeader(http.StatusNoContent)
		case parts[3] == "start":
			for _, p := range c.ports {
				if p == "8080" {
					http.Error(w, `{"message":"port is already allocated"}`, http.StatusInternalServerError)
					return
				}
			}
			c.running = true
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func serveDocker(t *testing.T, h http.Handler) *docker.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return docker.NewClient(sock)
}

func TestAddPortRestoresOldContainerWhenStartFails(t *testing.T) {
	d := &portDocker{containers: map[string]*fakeContainer{}}
	b := &DockerBackend{Client: serveDocker(t, d)}
	spec := &instance.Spec{ID: "web", Name: "web", Kind: instance.KindContainer,
		Container: &instance.ContainerSpec{ImageRef: "nginx", Ports: []instance.PortMapping{{HostPort: 80, GuestPort: 80, Protocol: "tcp"}}}}
	if err := b.Create(t.Context(), spec, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	if err := b.AddPort(t.Context(), spec, instance.PortMapping{HostPort: 8080, GuestPort: 80}); err == nil {
		t.Fatal("expected AddPort to fail")
	}
	if len(spec.Container.Ports) != 1 || spec.Container.Ports[0].HostPort != 80 {
		t.Errorf("ports not restored: %+v", spec.Container.Ports)
	}
	c, ok := d.containers[spec.Container.ContainerID]
	if len(d.containers) != 1 || !ok || !c.running || len(c.ports) != 1 || c.ports[0] != "80" {
		t.Errorf("spec points at %q, docker has %+v", spec.Container.ContainerID, d.containers)
	}
}

func TestFailedStopClearsExpectedExit(t *testing.T) {
	b := &DockerBackend{Client: serveDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))}
	spec := containerSpec("web", "c-web", instance.StateRunning)
	if err := b.Stop(t.Context(), spec, false, time.Second); err == nil {
		t.Fatal("expected Stop to fail")
	}
	if b.wasExpected("c-web") {
		t.Error("a failed stop left its expected-exit mark behind")
	}
}
