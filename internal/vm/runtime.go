//go:build linux

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// runtimeState is the per-instance state Reconcile uses to find and
// re-verify a process a previous anvild instance spawned.
type runtimeState struct {
	Pid       int       `json:"pid"`
	QMPSocket string    `json:"qmp_socket"`
	StartedAt time.Time `json:"started_at"`
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
