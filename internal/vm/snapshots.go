//go:build linux

package vm

import (
	"context"
	"fmt"
	"regexp"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// validSnapshotName restricts names to a safe token, since qemu-img's CLI
// args and the savevm/delvm HMP lines are built by string concatenation.
var validSnapshotName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func checkSnapshotName(name string) error {
	if !validSnapshotName.MatchString(name) {
		return fmt.Errorf("vm: snapshot name must be non-empty and contain only letters, digits, '_', '-', or '.' (got %q)", name)
	}
	return nil
}

// CreateSnapshot creates a named QCOW2 snapshot of spec's disk, live via
// QMP's savevm if running (no stop needed), or offline via qemu-img otherwise.
func (b *Backend) CreateSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: CreateSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	existing, err := image.ListSnapshots(spec.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: listing %s's existing snapshots: %w", spec.Name, err)
	}
	for _, s := range existing {
		if s.Name == name {
			return fmt.Errorf("vm: %s already has a snapshot named %q; delete it first", spec.Name, name)
		}
	}

	proc, running := b.lookup(spec.ID)

	if running && proc.QMP != nil {
		if _, err := proc.QMP.SaveVM(ctx, name); err != nil {
			return fmt.Errorf("vm: creating snapshot %q: %w", name, err)
		}
		return b.verifySnapshotPresence(spec, name, true, "creating")
	}

	return qemuImg(ctx, fmt.Sprintf("creating snapshot %q", name), "snapshot", "-c", name, spec.VM.DiskPath)
}

// RestoreSnapshot resets spec's disk only, never resuming a saved VM/RAM state.
// qemu-img can't touch a disk a running QEMU holds locked, so a running instance is stopped first, then restarted.
func (b *Backend) RestoreSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: RestoreSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	return restoreSnapshot(ctx, b, spec, name, func() error {
		return image.QemuImg(ctx, "snapshot", "-a", name, spec.VM.DiskPath)
	})
}

// DeleteSnapshot removes a previously created snapshot. Live if spec is
// running (QMP's delvm), otherwise via qemu-img's offline "snapshot -d".
func (b *Backend) DeleteSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: DeleteSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	proc, running := b.lookup(spec.ID)

	if running && proc.QMP != nil {
		if _, err := proc.QMP.DeleteVMSnapshot(ctx, name); err != nil {
			return fmt.Errorf("vm: deleting snapshot %q: %w", name, err)
		}
		return b.verifySnapshotPresence(spec, name, false, "deleting")
	}

	return qemuImg(ctx, fmt.Sprintf("deleting snapshot %q", name), "snapshot", "-d", name, spec.VM.DiskPath)
}

// ListSnapshots returns every snapshot currently recorded on spec's disk.
func (b *Backend) ListSnapshots(ctx context.Context, spec *instance.Spec) ([]instance.Snapshot, error) {
	if spec.VM == nil {
		return nil, fmt.Errorf("vm: ListSnapshots called with a nil VMSpec")
	}
	snaps, err := image.ListSnapshots(spec.VM.DiskPath)
	if err != nil {
		return nil, err
	}
	out := make([]instance.Snapshot, len(snaps))
	for i, s := range snaps {
		out[i] = instance.Snapshot{Name: s.Name, CreatedAt: s.CreatedAt, HasVMState: s.HasVMState}
	}
	return out, nil
}

// verifySnapshotPresence confirms a live savevm/delvm took effect by
// re-reading the disk's snapshot table, not trusting its HMP text output.
func (b *Backend) verifySnapshotPresence(spec *instance.Spec, name string, wantPresent bool, verb string) error {
	snaps, err := image.ListSnapshots(spec.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: verifying snapshot %q after %s it: %w", name, verb, err)
	}
	present := false
	for _, s := range snaps {
		if s.Name == name {
			present = true
			break
		}
	}
	if present != wantPresent {
		return fmt.Errorf("vm: %s snapshot %q on %s appeared to succeed but didn't take effect", verb, name, spec.Name)
	}
	return nil
}
