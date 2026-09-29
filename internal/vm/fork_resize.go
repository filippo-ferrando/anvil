//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"os/exec"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/image"
	"github.com/anvil-project/anvil/internal/vm/qemu"
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
	create := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", backing, diskPath, fmt.Sprint(virtualSize))
	if out, err := create.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: creating fork disk: %w: %s", err, out)
	}

	frozen := false
	if info, _ := b.GuestInfo(source); info.AgentConnected {
		say("freezing guest filesystems")
		err := b.agentFor(source.ID).Do(ctx, func(c *qemu.QGAConn) error { return c.FreezeFilesystems() })
		if err != nil {
			say(fmt.Sprintf("could not freeze guest filesystems (%v), copy is crash-consistent", err))
		} else {
			frozen = true
		}
	}
	jobID := "fork-" + source.ID
	startErr := proc.QMP.StartTopBackup(ctx, qemu.DiskDriveID, diskPath, jobID)
	if frozen {
		// The copy's point in time is fixed once the job started: thaw right away.
		thawErr := b.agentFor(source.ID).Do(context.WithoutCancel(ctx), func(c *qemu.QGAConn) error { return c.ThawFilesystems() })
		if thawErr != nil {
			log.Printf("vm: thawing %s's filesystems after fork: %v", source.Name, thawErr)
		}
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

	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running || proc.QMP == nil {
		cmd := exec.CommandContext(ctx, "qemu-img", "resize", spec.VM.DiskPath, fmt.Sprint(newBytes))
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("vm: resizing disk: %w: %s", err, out)
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
