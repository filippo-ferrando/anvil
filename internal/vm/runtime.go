//go:build linux

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
)

// runtimeState is the per-instance state Reconcile uses to find and
// re-verify a process a previous anvild instance spawned.
type runtimeState struct {
	Pid       int       `json:"pid"`
	QMPSocket string    `json:"qmp_socket"`
	StartedAt time.Time `json:"started_at"`

	// What the process was started with, which bounds live vCPU/memory changes.
	BootMemoryMiB int64 `json:"boot_memory_mib,omitempty"`
	MaxCPUs       int   `json:"max_cpus,omitempty"`
	MaxMemoryMiB  int64 `json:"max_memory_mib,omitempty"`

	// Jailed is set for a firecracker started under the jailer, whose files live in its jail.
	Jailed bool `json:"jailed,omitempty"`
	// JailExec is the firecracker binary the jail ran, whose file name is part of the jail path.
	JailExec string `json:"jail_exec,omitempty"`

	// Devices are the passthrough devices claimed for this run, released when it ends.
	Devices []hostdev.Claim `json:"devices,omitempty"`
}

func runtimeStatePath(instanceDir string) string {
	return filepath.Join(instanceDir, "runtime.json")
}

func saveRuntimeState(instanceDir string, rt runtimeState) error {
	data, err := json.Marshal(rt)
	if err != nil {
		return err
	}
	return os.WriteFile(runtimeStatePath(instanceDir), data, 0o640)
}

// loadRuntimeState returns (state, true, nil) if a runtime file exists
// and parses, (zero, false, nil) if there's none, or a read/parse error.
func loadRuntimeState(instanceDir string) (runtimeState, bool, error) {
	data, err := os.ReadFile(runtimeStatePath(instanceDir))
	if os.IsNotExist(err) {
		return runtimeState{}, false, nil
	}
	if err != nil {
		return runtimeState{}, false, err
	}
	var rt runtimeState
	if err := json.Unmarshal(data, &rt); err != nil {
		return runtimeState{}, false, err
	}
	return rt, true, nil
}

func removeRuntimeState(instanceDir string) {
	_ = os.Remove(runtimeStatePath(instanceDir))
}
