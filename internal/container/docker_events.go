package container

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
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

// ownersRefreshMin spaces out registry reads for events about unknown containers,
// which on a busy host are mostly containers anvil doesn't own.
const ownersRefreshMin = 5 * time.Second

// exitWatch is the DockerBackend state behind container exit detection.
type exitWatch struct {
	mu       sync.Mutex
	expected map[string]time.Time // container ID -> when anvil asked it to stop
	hook     func(instanceID string, state instance.State, reason string)
	once     sync.Once
	stop     context.CancelFunc

	owners   map[string]containerOwner // container ID -> its instance
	ownersAt time.Time                 // when owners was last rebuilt from the registry

	queue []exitReport // reports waiting for the hook, in event order
	wake  chan struct{}
}

type containerOwner struct {
	instanceID string
	name       string
	tracked    time.Time // set when Create added it, zero when read from the registry
}

type exitReport struct {
	instanceID string
	state      instance.State
	why        string
}

var _ instance.ExitNotifier = (*DockerBackend)(nil)

// SetExitHook starts following Docker's events, so a container that stops without
// anvil asking (crash, `docker stop`, dockerd restart) is reported through hook.
func (b *DockerBackend) SetExitHook(hook func(instanceID string, state instance.State, reason string)) {
	b.exits.mu.Lock()
	b.exits.hook = hook
	b.exits.mu.Unlock()
	if b.Registry == nil {
		return // no way to map a container back to its instance
	}
	b.exits.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		b.exits.stop = cancel
		b.exits.wake = make(chan struct{}, 1)
		go b.runHooks(ctx)
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
		_ = b.refreshOwners()
		b.reconcileExits(ctx)
		// since replays events from the reconcile onward, so an exit between the two is not lost.
		err := b.Client.Events(ctx, connected, map[string][]string{"type": {"container"}, "event": {"die", "start"}}, func(ev docker.Event) {
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

// report maps containerID to its instance and queues state for the hook.
func (b *DockerBackend) report(containerID string, state instance.State, why string) {
	b.exits.mu.Lock()
	hooked := b.exits.hook != nil
	b.exits.mu.Unlock()
	if !hooked {
		return
	}
	owner, ok := b.ownerOf(containerID)
	if !ok {
		return
	}
	if state != instance.StateRunning {
		log.Printf("docker: %s %s, now %s", owner.name, why, state)
	}
	b.exits.mu.Lock()
	b.exits.queue = append(b.exits.queue, exitReport{owner.instanceID, state, why})
	b.exits.mu.Unlock()
	select {
	case b.exits.wake <- struct{}{}:
	default:
	}
}

// runHooks passes queued reports to the hook one at a time, in order. The hook waits
// for the instance lock, so it runs here and not on the event stream's goroutine.
func (b *DockerBackend) runHooks(ctx context.Context) {
	for {
		b.exits.mu.Lock()
		batch, hook := b.exits.queue, b.exits.hook
		b.exits.queue = nil
		b.exits.mu.Unlock()
		for _, r := range batch {
			if hook != nil {
				hook(r.instanceID, r.state, r.why)
			}
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-b.exits.wake:
		}
	}
}

// trackContainer records a container anvil just created, so its events map to spec
// before the registry has the new ID.
func (b *DockerBackend) trackContainer(containerID string, spec *instance.Spec) {
	b.exits.mu.Lock()
	defer b.exits.mu.Unlock()
	if b.exits.owners == nil {
		b.exits.owners = make(map[string]containerOwner)
	}
	b.exits.owners[containerID] = containerOwner{instanceID: spec.ID, name: spec.Name, tracked: time.Now()}
}

// ownerOf finds containerID's instance in memory, rebuilding the map from the registry
// on a miss at most once every ownersRefreshMin.
func (b *DockerBackend) ownerOf(containerID string) (containerOwner, bool) {
	b.exits.mu.Lock()
	owner, ok := b.exits.owners[containerID]
	stale := time.Since(b.exits.ownersAt) >= ownersRefreshMin
	b.exits.mu.Unlock()
	if ok || !stale {
		return owner, ok
	}
	if err := b.refreshOwners(); err != nil {
		return containerOwner{}, false
	}
	b.exits.mu.Lock()
	defer b.exits.mu.Unlock()
	owner, ok = b.exits.owners[containerID]
	return owner, ok
}

// refreshOwners rebuilds the owner map from the registry, keeping containers Create
// tracked recently in case their record isn't saved yet.
func (b *DockerBackend) refreshOwners() error {
	specs, err := b.Registry.List(instance.KindContainer)
	if err != nil {
		return err
	}
	owners := make(map[string]containerOwner, len(specs))
	for _, spec := range specs {
		if spec.Container != nil && spec.Container.ContainerID != "" {
			owners[spec.Container.ContainerID] = containerOwner{instanceID: spec.ID, name: spec.Name}
		}
	}
	b.exits.mu.Lock()
	defer b.exits.mu.Unlock()
	for id, o := range b.exits.owners {
		if _, ok := owners[id]; !ok && !o.tracked.IsZero() && time.Since(o.tracked) < expectedExitWindow {
			owners[id] = o
		}
	}
	b.exits.owners, b.exits.ownersAt = owners, time.Now()
	return nil
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
