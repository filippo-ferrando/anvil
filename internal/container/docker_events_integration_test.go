package container

import (
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/container/docker"
	"github.com/anvil-project/anvil/internal/instance"
)

// TestRealDockerExitDetection runs a short-lived container on the real dockerd and checks
// that its crash, and a `docker stop` from outside anvil, are both noticed. Skips when
// dockerd isn't reachable or busybox:latest isn't already pulled (it never pulls).
func TestRealDockerExitDetection(t *testing.T) {
	client := docker.NewClient(docker.DefaultSocket)
	if ok, err := client.ImageExists(t.Context(), "busybox:latest"); err != nil || !ok {
		t.Skip("dockerd not reachable or busybox:latest not pulled, skipping")
	}

	create := func(name string, cmd ...string) *instance.Spec {
		t.Helper()
		id, err := client.CreateContainer(t.Context(), docker.CreateContainerParams{Name: name, Image: "busybox:latest", Cmd: cmd})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.RemoveContainer(t.Context(), id, true) })
		return containerSpec(name, id, instance.StateRunning)
	}
	crash := create("anvil-test-crash", "sh", "-c", "sleep 1; exit 3")
	// PID 1 ignores SIGTERM without a handler, so the trap makes `docker stop` end it cleanly.
	sleeper := create("anvil-test-sleep", "sh", "-c", "trap 'exit 0' TERM; sleep 300 & wait")

	b := &DockerBackend{Client: client, Registry: specList{crash, sleeper}}
	got := make(chan stateRecord, 8)
	b.SetExitHook(func(id string, state instance.State) { got <- stateRecord{id, state} })
	defer b.Close()
	time.Sleep(200 * time.Millisecond) // let the event stream connect

	for _, s := range []*instance.Spec{crash, sleeper} {
		if err := b.Start(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]instance.State{}
	deadline := time.After(20 * time.Second)
	stopped := false
	for len(seen) < 2 {
		select {
		case r := <-got:
			if r.state == instance.StateRunning {
				continue // the start events of the two Start calls above
			}
			seen[r.id] = r.state
			if r.id == crash.ID && !stopped {
				// Stop the other one behind anvil's back, like `docker stop` would.
				_ = client.StopContainer(t.Context(), sleeper.Container.ContainerID, 1)
				stopped = true
			}
		case <-deadline:
			t.Fatalf("timed out, reports so far: %v", seen)
		}
	}
	if seen[crash.ID] != instance.StateError {
		t.Errorf("crash: got %s, want error", seen[crash.ID])
	}
	if seen[sleeper.ID] != instance.StateStopped {
		t.Errorf("outside docker stop: got %s, want stopped", seen[sleeper.ID])
	}
}
