package container

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/anvil-project/anvil/internal/container/docker"
	"github.com/anvil-project/anvil/internal/instance"
)

// InstanceLister finds the instance behind a Docker container ID; *store.Store satisfies it.
type InstanceLister interface {
	List(kindFilter instance.Kind) ([]*instance.Spec, error)
}

// expectedExitWindow is how long after anvil stops or removes a container its "die"
// event is still taken as anvil's own doing, since Docker delivers events asynchronously.
const expectedExitWindow = 2 * time.Minute

const (
	eventRetryMin = time.Second
	eventRetryMax = 30 * time.Second
)

// exitWatch is the DockerBackend state behind container exit detection.
type exitWatch struct {
	mu       sync.Mutex
	expected map[string]time.Time // container ID -> when anvil asked it to stop
	hook     func(instanceID string, state instance.State)
	once     sync.Once
	stop     context.CancelFunc
}

var _ instance.ExitNotifier = (*DockerBackend)(nil)

// SetExitHook starts following Docker's events, so a container that stops without
// anvil asking (crash, `docker stop`, dockerd restart) is reported through hook.
func (b *DockerBackend) SetExitHook(hook func(instanceID string, state instance.State)) {
	b.exits.mu.Lock()
	b.exits.hook = hook
	b.exits.mu.Unlock()
	if b.Registry == nil {
		return // no way to map a container back to its instance
	}
	b.exits.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		b.exits.stop = cancel
		go b.watchEvents(ctx)
	})
}

// Close stops following Docker's events.
func (b *DockerBackend) Close() {
	if b.exits.stop != nil {
		b.exits.stop()
	}
}

// expectExit marks containerID as being stopped by anvil itself.
func (b *DockerBackend) expectExit(containerID string) {
	b.exits.mu.Lock()
	defer b.exits.mu.Unlock()
	if b.exits.expected == nil {
		b.exits.expected = make(map[string]time.Time)
	}
	for id, at := range b.exits.expected {
		if time.Since(at) > expectedExitWindow {
			delete(b.exits.expected, id) // e.g. a removed container that never sent "die"
		}
	}
	b.exits.expected[containerID] = time.Now()
}

// clearExpectedExit forgets a mark once the container runs again, so a later crash counts.
func (b *DockerBackend) clearExpectedExit(containerID string) {
	b.exits.mu.Lock()
	delete(b.exits.expected, containerID)
	b.exits.mu.Unlock()
}

// wasExpected reports (and consumes) a recent expectExit mark for containerID.
func (b *DockerBackend) wasExpected(containerID string) bool {
	b.exits.mu.Lock()
	defer b.exits.mu.Unlock()
	at, ok := b.exits.expected[containerID]
	if ok {
		delete(b.exits.expected, containerID)
	}
	return ok && time.Since(at) < expectedExitWindow
}

// watchEvents follows container "die" and "start" events, reconnecting with backoff when dockerd
// isn't reachable. Every (re)connect first catches up on exits missed meanwhile.
func (b *DockerBackend) watchEvents(ctx context.Context) {
	retry := eventRetryMin
	for ctx.Err() == nil {
		connected := time.Now()
		b.reconcileExits(ctx)
		err := b.Client.Events(ctx, map[string][]string{"type": {"container"}, "event": {"die", "start"}}, func(ev docker.Event) {
			switch ev.Action {
			case "die":
				b.handleDie(ev.Actor.ID, ev.Actor.Attributes["exitCode"])
			case "start":
				// Also fires for anvil's own starts; the manager only acts on a stopped/errored record.
				b.report(ev.Actor.ID, instance.StateRunning, "started outside anvil")
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(connected) > eventRetryMax {
			retry = eventRetryMin // the stream was up for a while: start the backoff over
		}
		log.Printf("docker: event stream: %v (retrying in %s)", err, retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry = min(retry*2, eventRetryMax)
	}
}

func (b *DockerBackend) handleDie(containerID, exitCode string) {
	if b.wasExpected(containerID) {
		return
	}
	code, err := strconv.Atoi(exitCode)
	if err != nil {
		code = -1
	}
	b.reportExit(containerID, code)
}

func (b *DockerBackend) reportExit(containerID string, exitCode int) {
	b.report(containerID, exitState(exitCode), fmt.Sprintf("exited on its own (code %d)", exitCode))
}

// reconcileExits reports every anvil container Docker no longer runs, e.g. after dockerd
// restarted while the event stream was down. The manager ignores ones already stopped.
func (b *DockerBackend) reconcileExits(ctx context.Context) {
	specs, err := b.Registry.List(instance.KindContainer)
	if err != nil {
		return
	}
	for _, spec := range specs {
		if spec.State != instance.StateRunning || spec.Container == nil || spec.Container.ContainerID == "" {
			continue
		}
		st, err := b.Client.InspectState(ctx, spec.Container.ContainerID)
		if err != nil {
			continue // dockerd unreachable, or the container is gone; nothing reliable to report
		}
		if st.Status == "exited" || st.Status == "dead" {
			if !b.wasExpected(spec.Container.ContainerID) {
				b.reportExit(spec.Container.ContainerID, st.ExitCode)
			}
		}
	}
}

// report maps containerID to its instance and passes state to the hook.
func (b *DockerBackend) report(containerID string, state instance.State, why string) {
	b.exits.mu.Lock()
	hook := b.exits.hook
	b.exits.mu.Unlock()
	if hook == nil {
		return
	}
	specs, err := b.Registry.List(instance.KindContainer)
	if err != nil {
		return
	}
	for _, spec := range specs {
		if spec.Container != nil && spec.Container.ContainerID == containerID {
			if spec.State != state {
				log.Printf("docker: %s %s, now %s", spec.Name, why, state)
			}
			hook(spec.ID, state)
			return
		}
	}
}

// exitState maps a container's exit status: 0, or death by SIGINT/SIGTERM (an outside
// `docker stop`), is a normal stop; anything else (crash, SIGKILL, OOM) is an error.
func exitState(code int) instance.State {
	switch code {
	case 0, 128 + 2, 128 + 15:
		return instance.StateStopped
	default:
		return instance.StateError
	}
}
