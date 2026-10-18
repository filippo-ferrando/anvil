//go:build linux

package vm

import (
	"log"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

var _ instance.ExitNotifier = (*Backend)(nil)

// instanceDir is config.InstanceDir, swappable so tests can keep instance files in a temp dir.
var instanceDir = config.InstanceDir

func (b *Backend) SetExitHook(hook func(instanceID string, state instance.State, reason string)) {
	b.mu.Lock()
	b.exitHook = hook
	b.mu.Unlock()
}

// watchExit waits for proc to exit. An exit not caused by Stop (guest poweroff, crash,
// OOM kill) releases the VM's resources and reports its new state through the exit hook.
func (b *Backend) watchExit(instanceID, networkMode string, proc *qemu.Process) {
	<-proc.Exited()

	b.mu.Lock()
	_, stopping := b.stopping[instanceID]
	current := b.running[instanceID] == proc
	hook := b.exitHook
	b.mu.Unlock()
	if stopping || !current {
		return // Stop is handling it, or a newer process replaced this one
	}

	state, reason := instance.StateStopped, "exited on its own (guest powered off)"
	if !proc.ExitedCleanly() {
		state, reason = instance.StateError, "QEMU exited on its own ("+proc.ExitStatus()+")"
	}
	log.Printf("vm: %s exited on its own, now %s", instanceID, state)
	b.releaseStopped(instanceID, networkMode, proc)
	if hook != nil {
		hook(instanceID, state, reason)
	}
}

// releaseStopped frees everything a VM held while it ran, once its QEMU is gone.
func (b *Backend) releaseStopped(instanceID, networkMode string, proc *qemu.Process) {
	b.unwatchGuest(instanceID)
	_ = proc.Close()
	b.killVirtiofsds(instanceID)
	if networkMode == "bridge" {
		// Best-effort cleanup; not worth failing over.
		if err := b.Networker.Detach(instanceID); err != nil {
			log.Printf("vm: failed to detach network device for %s: %v", instanceID, err)
		}
	}
	removeRuntimeState(instanceDir(instanceID))

	b.mu.Lock()
	if b.running[instanceID] == proc {
		delete(b.running, instanceID)
	}
	b.mu.Unlock()
}
