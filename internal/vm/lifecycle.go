//go:build linux

package vm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// vmProcess is what both hypervisor process types offer the shared lifecycle code.
type vmProcess interface {
	comparable
	Exited() <-chan struct{}
	Stop(ctx context.Context, timeout time.Duration) error
}

// vmTable tracks a backend's running VMs and its Start and Stop calls in progress.
// Its mu also guards the backend's other per-VM maps.
type vmTable[P vmProcess] struct {
	mu       sync.Mutex
	running  map[string]P        // instance ID -> live process
	starting map[string]struct{} // instance ID -> in-progress Start call
	stopping map[string]struct{} // instance ID -> Stop in progress, so its exit isn't reported
	exitHook func(instanceID string, state instance.State, reason string)
}

func newVMTable[P vmProcess]() vmTable[P] {
	return vmTable[P]{running: map[string]P{}, starting: map[string]struct{}{}, stopping: map[string]struct{}{}}
}

// lookup returns instanceID's process, if it runs.
func (t *vmTable[P]) lookup(instanceID string) (P, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.running[instanceID]
	return p, ok
}

// tracked reports whether instanceID has a process this daemon still tracks.
func (t *vmTable[P]) tracked(instanceID string) bool {
	_, ok := t.lookup(instanceID)
	return ok
}

// track records p as instanceID's running process.
func (t *vmTable[P]) track(instanceID string, p P) {
	t.mu.Lock()
	t.running[instanceID] = p
	t.mu.Unlock()
}

// beginStart marks a Start of instanceID in progress; done ends it. A VM already running
// gives running true and nothing to end, a concurrent Start an error.
func (t *vmTable[P]) beginStart(instanceID string) (running bool, done func(), err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.running[instanceID]; ok {
		return true, nil, nil
	}
	if _, ok := t.starting[instanceID]; ok {
		return false, nil, fmt.Errorf("vm: %s is already being started by a concurrent call", instanceID)
	}
	t.starting[instanceID] = struct{}{}
	return false, func() {
		t.mu.Lock()
		delete(t.starting, instanceID)
		t.mu.Unlock()
	}, nil
}

// beginStop marks a Stop of instanceID's running process, so its exit isn't reported.
// starting tells whether a Start is under way when nothing runs yet.
func (t *vmTable[P]) beginStop(instanceID string) (p P, ok, starting bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok = t.running[instanceID]
	_, starting = t.starting[instanceID]
	if ok {
		t.stopping[instanceID] = struct{}{}
	}
	return p, ok, starting
}

func (t *vmTable[P]) endStop(instanceID string) {
	t.mu.Lock()
	delete(t.stopping, instanceID)
	t.mu.Unlock()
}

func (t *vmTable[P]) SetExitHook(hook func(instanceID string, state instance.State, reason string)) {
	t.mu.Lock()
	t.exitHook = hook
	t.mu.Unlock()
}

// ownExit reports whether p's exit is one to report: no Stop caused it and no newer process
// replaced p. It also returns the exit hook, which may be nil.
func (t *vmTable[P]) ownExit(instanceID string, p P) (func(instanceID string, state instance.State, reason string), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, stopping := t.stopping[instanceID]
	return t.exitHook, !stopping && t.running[instanceID] == p
}

// minACPITimeout is how long ACPI still gets after a guest agent shutdown request ran out of time.
const minACPITimeout = 5 * time.Second

// gracefulStop asks the guest agent to shut down when it's connected, then falls back to
// proc.Stop with what's left of timeout. force or a zero timeout stops proc right away.
func (b *guestTracker) gracefulStop(ctx context.Context, spec *instance.Spec, proc interface {
	Exited() <-chan struct{}
	Stop(ctx context.Context, timeout time.Duration) error
}, force bool, timeout time.Duration) error {
	guest, _ := b.GuestInfo(spec)
	b.unwatchGuest(spec.ID)

	start := time.Now()
	graceful := !force && timeout > 0
	if graceful && guest.AgentConnected && b.agentShutdown(ctx, spec.ID, proc.Exited(), timeout) {
		return nil
	}
	stopTimeout := timeout - time.Since(start)
	if stopTimeout < minACPITimeout && timeout >= minACPITimeout {
		stopTimeout = minACPITimeout
	}
	if !graceful {
		stopTimeout = 0
	}
	return proc.Stop(ctx, stopTimeout)
}

// vmLifecycle is the part of a VM backend the restart helpers drive.
type vmLifecycle interface {
	Start(ctx context.Context, spec *instance.Spec) error
	Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error
	tracked(instanceID string) bool
}

// reseedAndRestart rebuilds spec's cloud-init seed for a mount change and restarts a
// running VM to apply it.
func reseedAndRestart(ctx context.Context, vm vmLifecycle, seeds *Backend, spec *instance.Spec) error {
	spec.VM.Generation++
	if err := seeds.buildSeed(spec); err != nil {
		return err
	}
	if !vm.tracked(spec.ID) {
		return nil
	}
	if err := vm.Stop(ctx, spec, false, 30*time.Second); err != nil {
		return fmt.Errorf("vm: stopping %s to apply mount change: %w", spec.Name, err)
	}
	if err := vm.Start(ctx, spec); err != nil {
		return fmt.Errorf("vm: restarting %s after mount change: %w", spec.Name, err)
	}
	return nil
}

// restoreSnapshot runs apply to put snapshot name back on spec's disk, stopping a running
// VM first and starting it again afterwards.
func restoreSnapshot(ctx context.Context, vm vmLifecycle, spec *instance.Spec, name string, apply func() error) error {
	running := vm.tracked(spec.ID)
	if running {
		if err := vm.Stop(ctx, spec, false, 30*time.Second); err != nil {
			return fmt.Errorf("vm: stopping %s to restore snapshot %q: %w", spec.Name, name, err)
		}
	}
	applyErr := apply()
	if running {
		if err := vm.Start(ctx, spec); err != nil {
			if applyErr != nil {
				return fmt.Errorf("vm: restoring snapshot %q: %w (restarting %s afterward also failed: %v)",
					name, applyErr, spec.Name, err)
			}
			return fmt.Errorf("vm: restarting %s after restoring snapshot %q: %w", spec.Name, name, err)
		}
	}
	if applyErr != nil {
		return fmt.Errorf("vm: restoring snapshot %q: %w", name, applyErr)
	}
	return nil
}
