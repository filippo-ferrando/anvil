//go:build linux

package vm

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// fakeHV is a hypervisor backend with only the base lifecycle, plus Fork.
type fakeHV struct{ started []string }

func (f *fakeHV) Create(context.Context, *instance.Spec, func(string)) error { return nil }
func (f *fakeHV) Start(_ context.Context, s *instance.Spec) error {
	f.started = append(f.started, s.ID)
	return nil
}
func (f *fakeHV) Stop(context.Context, *instance.Spec, bool, time.Duration) error { return nil }
func (f *fakeHV) Delete(context.Context, *instance.Spec) error                    { return nil }
func (f *fakeHV) Status(context.Context, *instance.Spec) (instance.State, error) {
	return instance.StateRunning, nil
}
func (f *fakeHV) Logs(context.Context, *instance.Spec, bool, int, func([]byte) error) error {
	return nil
}
func (f *fakeHV) Fork(context.Context, *instance.Spec, *instance.Spec, func(string)) error {
	return nil
}

func vmSpec(id string, hv instance.VMHypervisor) *instance.Spec {
	return &instance.Spec{ID: id, Kind: instance.KindVM, VM: &instance.VMSpec{Hypervisor: hv}}
}

func TestHypervisorsDispatch(t *testing.T) {
	ctx := context.Background()
	h := NewHypervisors(NewBackend(nil, nil, nil))

	// Unset and "qemu" both go to QEMU, which reports an untracked VM as stopped.
	for _, hv := range []instance.VMHypervisor{"", instance.VMHypervisorQEMU} {
		if st, err := h.Status(ctx, vmSpec("a", hv)); err != nil || st != instance.StateStopped {
			t.Fatalf("hypervisor %q: Status = %v, %v; want stopped from QEMU", hv, st, err)
		}
	}

	if err := h.Start(ctx, vmSpec("b", instance.VMHypervisorFirecracker)); err == nil || !strings.Contains(err.Error(), "isn't implemented yet") {
		t.Fatalf("firecracker without a backend: err = %v", err)
	}
	if err := h.Start(ctx, vmSpec("c", "xen")); err == nil || !strings.Contains(err.Error(), "unknown hypervisor") {
		t.Fatalf("unknown hypervisor: err = %v", err)
	}

	fc := &fakeHV{}
	h.Firecracker = fc
	if err := h.Start(ctx, vmSpec("d", instance.VMHypervisorFirecracker)); err != nil || len(fc.started) != 1 {
		t.Fatalf("Start on firecracker: err = %v, started %v", err, fc.started)
	}
	if err := h.CreateSnapshot(ctx, vmSpec("e", instance.VMHypervisorFirecracker), "s"); err == nil ||
		err.Error() != "vm: snapshots isn't supported on firecracker" {
		t.Fatalf("missing capability: err = %v", err)
	}
	if err := h.ExportDisk(ctx, vmSpec("f", instance.VMHypervisorFirecracker), "/x"); err == nil {
		t.Fatal("ExportDisk on firecracker should fail until it's supported")
	}
	if err := h.Fork(ctx, vmSpec("src", instance.VMHypervisorFirecracker), vmSpec("g", instance.VMHypervisorFirecracker), nil); err != nil {
		t.Fatalf("Fork on firecracker: %v", err)
	}
}
