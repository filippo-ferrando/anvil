package container

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/container/docker"
	"github.com/anvil-project/anvil/internal/instance"
)

// fakeDocker serves /events from a channel and container state from a map.
type fakeDocker struct {
	events chan map[string]any
	mu     sync.Mutex
	states map[string]map[string]any // container ID -> inspect State
}

func startFakeDocker(t *testing.T) (*fakeDocker, *docker.Client) {
	t.Helper()
	fd := &fakeDocker{events: make(chan map[string]any, 8), states: map[string]map[string]any{}}
	dir, err := os.MkdirTemp("", "anvil-docker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case ev := <-fd.events:
					_ = json.NewEncoder(w).Encode(ev)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			id := filepath.Base(filepath.Dir(r.URL.Path))
			fd.mu.Lock()
			st, ok := fd.states[id]
			fd.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"State": st})
		case strings.HasSuffix(r.URL.Path, "/stop"), strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return fd, docker.NewClient(sock)
}

func (fd *fakeDocker) die(id string, code string) {
	fd.events <- map[string]any{"Type": "container", "Action": "die", "Actor": map[string]any{"ID": id, "Attributes": map[string]string{"exitCode": code}}}
}

type specList []*instance.Spec

func (l specList) List(instance.Kind) ([]*instance.Spec, error) { return l, nil }

type stateRecord struct {
	id    string
	state instance.State
}

func watchedBackend(t *testing.T, specs specList) (*fakeDocker, *DockerBackend, chan stateRecord) {
	t.Helper()
	fd, client := startFakeDocker(t)
	b := &DockerBackend{Client: client, Registry: specs}
	got := make(chan stateRecord, 8)
	b.SetExitHook(func(id string, state instance.State) { got <- stateRecord{id, state} })
	t.Cleanup(b.Close)
	return fd, b, got
}

func nextRecord(t *testing.T, got chan stateRecord) stateRecord {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a state report")
		return stateRecord{}
	}
}

func noRecord(t *testing.T, got chan stateRecord) {
	t.Helper()
	select {
	case r := <-got:
		t.Fatalf("expected no report, got %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
}

func containerSpec(id, containerID string, state instance.State) *instance.Spec {
	return &instance.Spec{ID: id, Name: id, Kind: instance.KindContainer, State: state,
		Container: &instance.ContainerSpec{ContainerID: containerID}}
}

func TestCrashOutsideAnvilIsReported(t *testing.T) {
	fd, _, got := watchedBackend(t, specList{containerSpec("web", "c-web", instance.StateRunning)})
	fd.die("c-web", "1")
	if r := nextRecord(t, got); r.id != "web" || r.state != instance.StateError {
		t.Errorf("got %+v, want web error", r)
	}
	fd.die("c-other", "0") // not an anvil container
	noRecord(t, got)
}

func TestOutsideDockerStopIsAStop(t *testing.T) {
	fd, _, got := watchedBackend(t, specList{containerSpec("web", "c-web", instance.StateRunning)})
	fd.die("c-web", "143") // SIGTERM from `docker stop`
	if r := nextRecord(t, got); r.state != instance.StateStopped {
		t.Errorf("got %+v, want stopped", r)
	}
}

func TestAnvilStopIsNotReported(t *testing.T) {
	spec := containerSpec("web", "c-web", instance.StateRunning)
	fd, b, got := watchedBackend(t, specList{spec})
	if err := b.Stop(t.Context(), spec, false, time.Second); err != nil {
		t.Fatal(err)
	}
	fd.die("c-web", "143")
	noRecord(t, got)

	// After anvil starts it again, a crash is reported as usual.
	if err := b.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	fd.die("c-web", "139")
	if r := nextRecord(t, got); r.state != instance.StateError {
		t.Errorf("got %+v, want error", r)
	}
}

func TestStartOutsideAnvilIsReported(t *testing.T) {
	fd, _, got := watchedBackend(t, specList{containerSpec("web", "c-web", instance.StateStopped)})
	fd.events <- map[string]any{"Type": "container", "Action": "start", "Actor": map[string]any{"ID": "c-web"}}
	if r := nextRecord(t, got); r.id != "web" || r.state != instance.StateRunning {
		t.Errorf("got %+v, want web running", r)
	}
}

func TestExitsMissedWhileDisconnectedAreReconciled(t *testing.T) {
	fd, client := startFakeDocker(t)
	fd.states["c-web"] = map[string]any{"Status": "exited", "ExitCode": 137}
	fd.states["c-db"] = map[string]any{"Status": "running", "Running": true}
	b := &DockerBackend{Client: client, Registry: specList{
		containerSpec("web", "c-web", instance.StateRunning),
		containerSpec("db", "c-db", instance.StateRunning),
	}}
	got := make(chan stateRecord, 8)
	b.SetExitHook(func(id string, state instance.State) { got <- stateRecord{id, state} })
	defer b.Close()

	if r := nextRecord(t, got); r.id != "web" || r.state != instance.StateError {
		t.Errorf("got %+v, want web error", r)
	}
	noRecord(t, got)
}

func TestExitState(t *testing.T) {
	for code, want := range map[int]instance.State{0: instance.StateStopped, 143: instance.StateStopped, 130: instance.StateStopped,
		1: instance.StateError, 137: instance.StateError, -1: instance.StateError} {
		if got := exitState(code); got != want {
			t.Errorf("exitState(%d) = %s, want %s", code, got, want)
		}
	}
}
