//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

var _ instance.Resizer = (*Backend)(nil)

const bytesPerGiB = 1 << 30

// forkLive copies a running source's disk into diskPath with a QEMU backup job: the copy
// is exactly the disk at the moment the job starts, while the VM keeps running. With a
// guest agent the filesystems are frozen around that moment, so the copy is also clean
// at the filesystem level, not just like a sudden power loss.
func (b *Backend) forkLive(ctx context.Context, source *instance.Spec, proc *qemu.Process, backing, diskPath string, progress func(string)) error {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	_, virtualSize, err := image.DiskUsage(source.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: inspecting %s's disk: %w", source.Name, err)
	}
	if err := qemuImg(ctx, "creating fork disk", "create", "-f", "qcow2", "-F", "qcow2", "-b", backing, diskPath, fmt.Sprint(virtualSize)); err != nil {
		return err
	}

	frozen, triedFreeze := false, false
	if info, _ := b.GuestInfo(source); info.AgentConnected {
		say("freezing guest filesystems")
		triedFreeze = true
		err := b.agentFor(source.ID).Do(ctx, func(c *qemu.QGAConn) error { return c.FreezeFilesystems() })
		if err != nil {
			say(fmt.Sprintf("could not freeze guest filesystems (%v), copy is crash-consistent", err))
		} else {
			frozen = true
		}
	}
	jobID := "fork-" + source.ID
	startErr := proc.QMP.StartTopBackup(ctx, qemu.DiskDriveID, diskPath, jobID)
	if triedFreeze {
		// A failed or cancelled freeze can still leave the guest frozen, so always thaw.
		b.thawAfterFork(ctx, source, frozen)
	}
	if startErr != nil {
		return fmt.Errorf("vm: forking disk: %w", startErr)
	}
	mode := "crash-consistent"
	if frozen {
		mode = "filesystem-consistent"
	}
	say("copying disk live (" + mode + ")")
	err = proc.QMP.FinishTopBackup(ctx, jobID, func(pct int) {
		say(fmt.Sprintf("copying disk: %d%%", pct))
	})
	if err != nil {
		return fmt.Errorf("vm: forking disk: %w", err)
	}
	return nil
}

// forkThawTimeout bounds the thaw after a fork's freeze, which runs even when ctx is done.
const forkThawTimeout = 45 * time.Second

// thawAfterFork thaws source's filesystems. An error only matters when the freeze was
// known to work: after a failed freeze the guest is most likely not frozen.
func (b *Backend) thawAfterFork(ctx context.Context, source *instance.Spec, frozen bool) {
	thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forkThawTimeout)
	defer cancel()
	err := b.agentFor(source.ID).Do(thawCtx, func(c *qemu.QGAConn) error { return c.ThawFilesystems() })
	if err != nil && frozen {
		log.Printf("vm: thawing %s's filesystems after fork: %v", source.Name, err)
	}
}

// ResizeDisk grows spec's disk to newGiB. A running VM grows live, and with a guest agent
// its root partition and filesystem grow right away; otherwise at the next boot, where
// cloud-init's growpart and resizefs run on every start.
func (b *Backend) ResizeDisk(ctx context.Context, spec *instance.Spec, newGiB int64) (string, error) {
	if spec.VM == nil || spec.VM.DiskPath == "" {
		return "", fmt.Errorf("vm: %s has no disk to resize", spec.Name)
	}
	_, current, err := image.DiskUsage(spec.VM.DiskPath)
	if err != nil {
		return "", fmt.Errorf("vm: inspecting %s's disk: %w", spec.Name, err)
	}
	newBytes := newGiB * bytesPerGiB
	if newBytes <= current {
		return "", fmt.Errorf("vm: disk can only grow: %s is already %.1f GiB", spec.Name, float64(current)/bytesPerGiB)
	}

	proc, running := b.lookup(spec.ID)
	if !running || proc.QMP == nil {
		if err := qemuImg(ctx, "resizing disk", "resize", spec.VM.DiskPath, fmt.Sprint(newBytes)); err != nil {
			return "", err
		}
		return fmt.Sprintf("disk grown to %d GiB; the guest filesystem grows at the next boot", newGiB), nil
	}

	if err := proc.QMP.BlockResize(ctx, qemu.DiskDriveID, newBytes); err != nil {
		return "", fmt.Errorf("vm: %w", err)
	}
	note := fmt.Sprintf("disk grown live to %d GiB", newGiB)
	if info, _ := b.GuestInfo(spec); info.AgentConnected {
		if _, err := b.guestExec(ctx, spec.ID, growGuestFSScript); err == nil {
			return note + "; guest partition and filesystem grown", nil
		}
	}
	return note + "; the guest filesystem grows at the next boot", nil
}

// growGuestFSScript runs cloud-init's own growpart and resizefs modules, which know
// every catalog distro's root layout.
const growGuestFSScript = "cloud-init single --name growpart --frequency always && " +
	"cloud-init single --name resizefs --frequency always"

// Fork copies source's disk into dest's dir, preserving the backing-file pointer. A running
// source is copied live and point-in-time (see forkLive); a stopped one with qemu-img.
func (b *Backend) Fork(ctx context.Context, source, dest *instance.Spec, progress func(status string)) error {
	if source.VM == nil || dest.VM == nil {
		return fmt.Errorf("vm: Fork called with a nil VMSpec")
	}

	backing, err := image.BackingFile(source.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: inspecting %s's disk: %w", source.Name, err)
	}
	if backing == "" {
		return fmt.Errorf("vm: %s's disk has no backing file to preserve", source.Name)
	}

	dir := instanceDir(dest.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}
	diskPath := filepath.Join(dir, "disk.qcow2")

	proc, running := b.lookup(source.ID)
	if running && proc.QMP != nil {
		if err := b.forkLive(ctx, source, proc, backing, diskPath, progress); err != nil {
			_ = os.Remove(diskPath)
			return err
		}
	} else {
		if progress != nil {
			progress("copying disk")
		}
		if !reflinkFork(ctx, source.VM.DiskPath, diskPath) {
			// -U still opens a disk some untracked QEMU might hold locked.
			if err := qemuImg(ctx, "forking disk", "convert", "-U", "-O", "qcow2",
				"-o", "backing_file="+backing+",backing_fmt=qcow2", source.VM.DiskPath, diskPath); err != nil {
				return err
			}
		}
	}
	dest.VM.DiskPath = diskPath

	if progress != nil {
		progress("building cloud-init seed")
	}
	return b.buildSeed(dest)
}

// reflinkFork makes dst a copy-on-write clone of a stopped source's disk, in one call
// whatever its size, where the filesystem can. A clone keeps the backing file's name as
// written, so only an absolute one qualifies, and the source's internal snapshots are
// dropped, as a convert leaves them out. False leaves nothing behind, for a convert instead.
func reflinkFork(ctx context.Context, src, dst string) bool {
	if backing, err := image.BackingFile(src); err != nil || !filepath.IsAbs(backing) {
		return false
	}
	if reflink(src, dst) != nil {
		return false
	}
	snaps, err := image.ListSnapshots(dst)
	for _, s := range snaps {
		if err != nil {
			break
		}
		err = qemuImg(ctx, "dropping the source's snapshots", "snapshot", "-d", s.Name, dst)
	}
	if err != nil {
		_ = os.Remove(dst)
		return false
	}
	return true
}
