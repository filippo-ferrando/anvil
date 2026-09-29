package instance

import (
	"context"
	"errors"
	"fmt"
)

// Resizer is implemented by a Backend that can grow an instance's disk (VM only).
// It returns a short note on how the change was applied.
type Resizer interface {
	ResizeDisk(ctx context.Context, spec *Spec, newGiB int64) (note string, err error)
}

// ErrNeedsRestart marks a change a backend can't apply to the running instance; it
// takes effect at the next start instead. The wrapped message says why.
var ErrNeedsRestart = errors.New("applies at the next start")

// LiveResizer is implemented by a Backend that can change a running VM's vCPUs and
// memory. Each returns a short note, or an error wrapping ErrNeedsRestart.
type LiveResizer interface {
	SetCPUsLive(ctx context.Context, spec *Spec, cpus int) (note string, err error)
	SetMemoryLive(ctx context.Context, spec *Spec, memoryMiB int64) (note string, err error)
}

// UpdateParams is Manager.Update's input; nil fields stay unchanged.
type UpdateParams struct {
	CPUs          *int
	MemoryMiB     *int64
	DiskGiB       *int64
	Autostart     *bool
	RestartPolicy *RestartPolicy
}

// UpdateResult says what Update did.
type UpdateResult struct {
	Spec           *Spec
	RestartPending bool     // CPU/memory changes wait for the running VM's next start
	Notes          []string // one line per applied change
}

// Minimums for VM resources set through Update.
const (
	minCPUs      = 1
	minMemoryMiB = 128
)

// Update changes name's settings. Disk growth is applied right away (live on a running VM).
// CPU and memory of a running VM change live when its backend can; otherwise at its next start.
func (m *Manager) Update(ctx context.Context, name string, p UpdateParams) (UpdateResult, error) {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return UpdateResult{}, err
	}
	if (p.CPUs != nil || p.MemoryMiB != nil || p.DiskGiB != nil) && spec.VM == nil {
		return UpdateResult{}, fmt.Errorf("instance: CPU, memory and disk can only be changed on a VM")
	}
	running := spec.State == StateRunning
	live, _ := m.backends[spec.Kind].(LiveResizer)
	var res UpdateResult

	// applyLive runs a live change on a running VM and records how it went.
	applyLive := func(fallback string, change func() (string, error)) error {
		if !running {
			res.Notes = append(res.Notes, fallback)
			return nil
		}
		if live == nil {
			res.Notes = append(res.Notes, fallback+" (applies at the next start)")
			res.RestartPending = true
			return nil
		}
		note, err := change()
		switch {
		case err == nil:
			res.Notes = append(res.Notes, note)
		case errors.Is(err, ErrNeedsRestart):
			res.Notes = append(res.Notes, fmt.Sprintf("%s (%v)", fallback, err))
			res.RestartPending = true
		default:
			return err
		}
		return nil
	}

	if p.CPUs != nil && *p.CPUs != spec.VM.CPUs {
		n := *p.CPUs
		if n < minCPUs {
			return UpdateResult{}, fmt.Errorf("instance: cpus must be at least %d", minCPUs)
		}
		err := applyLive(fmt.Sprintf("cpus set to %d", n), func() (string, error) { return live.SetCPUsLive(ctx, spec, n) })
		if err != nil {
			return UpdateResult{}, err
		}
		spec.VM.CPUs = n
	}
	if p.MemoryMiB != nil && *p.MemoryMiB != spec.VM.MemoryMiB {
		mib := *p.MemoryMiB
		if mib < minMemoryMiB {
			return UpdateResult{}, fmt.Errorf("instance: memory must be at least %d MiB", minMemoryMiB)
		}
		err := applyLive(fmt.Sprintf("memory set to %d MiB", mib), func() (string, error) { return live.SetMemoryLive(ctx, spec, mib) })
		if err != nil {
			return UpdateResult{}, err
		}
		spec.VM.MemoryMiB = mib
	}
	if p.DiskGiB != nil {
		b, err := m.backendFor(spec.Kind)
		if err != nil {
			return UpdateResult{}, err
		}
		rz, ok := b.(Resizer)
		if !ok {
			return UpdateResult{}, fmt.Errorf("instance: %s instances can't be resized", spec.Kind)
		}
		note, err := rz.ResizeDisk(ctx, spec, *p.DiskGiB)
		if err != nil {
			return UpdateResult{}, err
		}
		spec.VM.DiskGiB = *p.DiskGiB
		res.Notes = append(res.Notes, note)
	}
	if p.Autostart != nil && *p.Autostart != spec.Autostart {
		spec.Autostart = *p.Autostart
		res.Notes = append(res.Notes, fmt.Sprintf("autostart %s", onOff(*p.Autostart)))
	}
	if p.RestartPolicy != nil && *p.RestartPolicy != spec.RestartPolicy {
		spec.RestartPolicy = *p.RestartPolicy
		res.Notes = append(res.Notes, "restart policy set to "+p.RestartPolicy.String())
	}

	if err := m.registry.PutInstance(spec); err != nil {
		return UpdateResult{}, err
	}
	res.Spec = m.withGuest(spec)
	return res, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
