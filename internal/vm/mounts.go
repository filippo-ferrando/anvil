//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anvil-project/anvil/internal/hostpath"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

// mountFSVirtiofs marks a VMSpec whose guest fstab was written for virtiofs. Specs from
// before virtiofs (9p) have it empty and get their fstab rewritten on their next start.
const mountFSVirtiofs = "virtiofs"

// mountsGenMarker records, inside the guest, which Generation last rewrote anvil's fstab lines.
const mountsGenMarker = "/var/lib/anvil-mounts.gen"

// anvilFstabPattern (sed -E) matches every fstab line anvil ever wrote for a mount, 9p or virtiofs.
const anvilFstabPattern = `^mount[0-9]+[[:space:]].*[[:space:]](9p|virtiofs)[[:space:]]`

// unplugTimeout bounds how long a live umount waits for the guest to release the device.
// Linux's PCIe hot-plug driver alone waits 5 seconds before it does.
const unplugTimeout = 20 * time.Second

func virtiofsSocket(instanceID, tag string) string {
	return filepath.Join(instanceDir(instanceID), "fs-"+tag+".sock")
}

func virtiofsLog(instanceID, tag string) string {
	return filepath.Join(instanceDir(instanceID), "virtiofsd-"+tag+".log")
}

// fstabLine is m's /etc/fstab entry; spaces in the guest path are escaped the fstab way.
func fstabLine(m instance.Mount) string {
	opts := "defaults,nofail"
	if m.ReadOnly {
		opts += ",ro"
	}
	path := strings.NewReplacer(" ", `\040`, "\t", `\011`).Replace(m.GuestPath)
	return fmt.Sprintf("%s %s virtiofs %s 0 0", m.Tag, path, opts)
}

// mergeMounts adds a bootcmd that keeps the guest's fstab in line with mounts. The fstab is
// only rewritten once per generation, so mounts changed live since then survive reboots.
func mergeMounts(existing string, mounts []instance.Mount, generation int) (string, error) {
	if len(mounts) == 0 && generation == 0 {
		return existing, nil
	}
	doc, err := parseCloudConfig(existing)
	if err != nil {
		return "", err
	}

	gen := fmt.Sprint(generation)
	lines := []string{
		"if ! grep -qx " + gen + " " + mountsGenMarker + " 2>/dev/null; then",
		"  sed -E -i '/" + anvilFstabPattern + "/d' /etc/fstab",
	}
	for _, m := range mounts {
		lines = append(lines, "  echo "+shQuote(fstabLine(m))+" >> /etc/fstab")
	}
	lines = append(lines, "  mkdir -p "+filepath.Dir(mountsGenMarker)+" && echo "+gen+" > "+mountsGenMarker, "fi")
	for _, m := range mounts {
		q := shQuote(m.GuestPath)
		lines = append(lines, "mkdir -p "+q+"; mountpoint -q "+q+" || mount "+q+" || true")
	}

	bootcmd, _ := doc["bootcmd"].([]any)
	doc["bootcmd"] = append([]any{strings.Join(lines, "\n")}, bootcmd...)
	return encodeCloudConfig(doc)
}

// mountLiveScript mounts m in a running guest right after its device was hot-plugged,
// retrying while the guest kernel is still bringing the device up.
func mountLiveScript(m instance.Mount) string {
	q := shQuote(m.GuestPath)
	return strings.Join([]string{
		"set -e",
		"mkdir -p " + q,
		"sed -E -i '/^" + m.Tag + "[[:space:]]/d' /etc/fstab",
		"echo " + shQuote(fstabLine(m)) + " >> /etc/fstab",
		"i=0",
		"until mount " + q + " 2>/dev/null; do",
		"  i=$((i+1))",
		"  if [ $i -ge 40 ]; then mount " + q + "; fi",
		"  sleep 0.25",
		"done",
	}, "\n")
}

// umountLiveScript unmounts m in a running guest and drops its fstab line.
func umountLiveScript(m instance.Mount) string {
	q := shQuote(m.GuestPath)
	return strings.Join([]string{
		"set -e",
		"if mountpoint -q " + q + "; then umount " + q + "; fi",
		"sed -E -i '/^" + m.Tag + "[[:space:]]/d' /etc/fstab",
	}, "\n")
}

// Mount shares hostPath into spec's guest at guestPath over virtiofs. A running VM with a
// guest agent gets it live; otherwise the seed is rebuilt and a running VM restarts.
func (b *Backend) Mount(ctx context.Context, spec *instance.Spec, hostPath, guestPath string, readOnly bool) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Mount called with a nil VMSpec")
	}
	v := spec.VM

	for _, m := range v.Mounts {
		if m.GuestPath == guestPath {
			return fmt.Errorf("vm: %s already has a mount at %s", spec.Name, guestPath)
		}
	}
	if len(v.Mounts) >= qemu.HotplugPorts {
		return fmt.Errorf("vm: %s already has the maximum of %d mounts", spec.Name, qemu.HotplugPorts)
	}
	if info, err := os.Stat(hostPath); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("vm: host path %s: %w%s", hostPath, err, hostpath.Hint(hostPath))
		}
		return fmt.Errorf("vm: host path %s: %w", hostPath, err)
	} else if !info.IsDir() {
		return fmt.Errorf("vm: host path %s is not a directory", hostPath)
	}

	m := instance.Mount{
		HostPath:  hostPath,
		GuestPath: guestPath,
		Tag:       fmt.Sprintf("mount%d", v.NextMountIndex),
		ReadOnly:  readOnly,
	}
	if proc, ok := b.liveMountTarget(ctx, spec); ok {
		if err := b.mountLive(ctx, spec, proc, m); err != nil {
			return err
		}
		v.NextMountIndex++
		v.Mounts = append(v.Mounts, m)
		v.MountFS = mountFSVirtiofs
		return nil
	}

	v.NextMountIndex++
	v.Mounts = append(v.Mounts, m)
	return b.reconfigureAndRestartIfRunning(ctx, spec)
}

// Umount removes a mount previously added with Mount, identified by its guest path.
func (b *Backend) Umount(ctx context.Context, spec *instance.Spec, guestPath string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Umount called with a nil VMSpec")
	}
	v := spec.VM

	idx := -1
	for i, m := range v.Mounts {
		if m.GuestPath == guestPath {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("vm: %s has no mount at %s", spec.Name, guestPath)
	}
	m := v.Mounts[idx]

	if proc, ok := b.liveMountTarget(ctx, spec); ok {
		if err := b.umountLive(ctx, spec, proc, m); err != nil {
			return err
		}
		v.Mounts = append(v.Mounts[:idx], v.Mounts[idx+1:]...)
		return nil
	}

	v.Mounts = append(v.Mounts[:idx], v.Mounts[idx+1:]...)
	return b.reconfigureAndRestartIfRunning(ctx, spec)
}

// liveMountTarget returns spec's QEMU process when a mount change can be applied live:
// the VM runs, its mounts already use virtiofs, and the guest agent can run commands.
func (b *Backend) liveMountTarget(ctx context.Context, spec *instance.Spec) (*qemu.Process, bool) {
	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running || proc.QMP == nil {
		return nil, false
	}
	if spec.VM.MountFS != mountFSVirtiofs && len(spec.VM.Mounts) > 0 {
		return nil, false // 9p-era fstab: a restart rewrites it for virtiofs
	}
	if info, _ := b.GuestInfo(spec); !info.AgentConnected {
		return nil, false
	}
	if _, err := b.guestExec(ctx, spec.ID, "true"); err != nil {
		return nil, false // e.g. guest-exec blocked by the distro's agent policy
	}
	return proc, true
}

// guestExec runs script with /bin/sh in the guest, failing on a non-zero exit.
func (b *Backend) guestExec(ctx context.Context, instanceID, script string) (qemu.ExecResult, error) {
	var res qemu.ExecResult
	err := b.agentFor(instanceID).Do(ctx, func(c *qemu.QGAConn) error {
		var err error
		res, err = c.Exec(ctx, "/bin/sh", "-c", script)
		return err
	})
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr+" "+res.Stdout))
	}
	return res, nil
}

func (b *Backend) mountLive(ctx context.Context, spec *instance.Spec, proc *qemu.Process, m instance.Mount) error {
	sock := virtiofsSocket(spec.ID, m.Tag)
	d, err := qemu.StartVirtiofsd(m.HostPath, sock, m.ReadOnly, virtiofsLog(spec.ID, m.Tag))
	if err != nil {
		return fmt.Errorf("vm: %w", err)
	}
	if err := proc.QMP.AddVirtiofs(ctx, m.Tag, sock); err != nil {
		d.Kill()
		return fmt.Errorf("vm: %w", err)
	}
	if _, err := b.guestExec(ctx, spec.ID, mountLiveScript(m)); err != nil {
		if rmErr := proc.QMP.RemoveVirtiofs(context.WithoutCancel(ctx), m.Tag, unplugTimeout); rmErr != nil {
			log.Printf("vm: rolling back virtiofs device %s on %s: %v", m.Tag, spec.Name, rmErr)
		}
		d.Kill()
		return fmt.Errorf("vm: mounting %s in the guest: %w", m.GuestPath, err)
	}
	b.trackVirtiofsd(spec.ID, m.Tag, d)
	return nil
}

func (b *Backend) umountLive(ctx context.Context, spec *instance.Spec, proc *qemu.Process, m instance.Mount) error {
	if _, err := b.guestExec(ctx, spec.ID, umountLiveScript(m)); err != nil {
		return fmt.Errorf("vm: unmounting %s in the guest (still in use?): %w", m.GuestPath, err)
	}
	if err := proc.QMP.RemoveVirtiofs(ctx, m.Tag, unplugTimeout); err != nil {
		return fmt.Errorf("vm: %w", err)
	}
	if d := b.untrackVirtiofsd(spec.ID, m.Tag); d != nil {
		// virtiofsd exits by itself once QEMU drops the connection.
		if !d.WaitExit(ctx, 2*time.Second) {
			d.Kill()
		}
	}
	return nil
}

// startVirtiofsds starts one virtiofsd per mount of spec, before QEMU connects to them.
func (b *Backend) startVirtiofsds(spec *instance.Spec) ([]qemu.Mount, error) {
	var mounts []qemu.Mount
	for _, m := range spec.VM.Mounts {
		sock := virtiofsSocket(spec.ID, m.Tag)
		d, err := qemu.StartVirtiofsd(m.HostPath, sock, m.ReadOnly, virtiofsLog(spec.ID, m.Tag))
		if err != nil {
			b.killVirtiofsds(spec.ID)
			if _, statErr := os.Stat(m.HostPath); os.IsPermission(statErr) {
				return nil, fmt.Errorf("vm: %w%s", err, hostpath.Hint(m.HostPath))
			}
			return nil, fmt.Errorf("vm: %w", err)
		}
		b.trackVirtiofsd(spec.ID, m.Tag, d)
		mounts = append(mounts, qemu.Mount{Tag: m.Tag, SocketPath: sock})
	}
	return mounts, nil
}

func (b *Backend) trackVirtiofsd(instanceID, tag string, d *qemu.Virtiofsd) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.virtiofs[instanceID] == nil {
		b.virtiofs[instanceID] = make(map[string]*qemu.Virtiofsd)
	}
	b.virtiofs[instanceID][tag] = d
}

func (b *Backend) untrackVirtiofsd(instanceID, tag string) *qemu.Virtiofsd {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.virtiofs[instanceID][tag]
	delete(b.virtiofs[instanceID], tag)
	return d
}

// killVirtiofsds stops every virtiofsd of instanceID. They normally exit with QEMU;
// this covers a QEMU that never connected, or one still shutting down.
func (b *Backend) killVirtiofsds(instanceID string) {
	b.mu.Lock()
	daemons := b.virtiofs[instanceID]
	delete(b.virtiofs, instanceID)
	b.mu.Unlock()
	for _, d := range daemons {
		d.Kill()
	}
}

// upgradeLegacyMounts moves a VM set up with 9p mounts to virtiofs: bumping Generation
// makes cloud-init run the mounts bootcmd again, which rewrites the guest's fstab.
func (b *Backend) upgradeLegacyMounts(spec *instance.Spec) error {
	v := spec.VM
	if len(v.Mounts) == 0 || v.MountFS == mountFSVirtiofs {
		return nil
	}
	v.Generation++
	v.MountFS = mountFSVirtiofs
	if err := b.buildSeed(spec); err != nil {
		return fmt.Errorf("vm: rewriting %s's mounts for virtiofs: %w", spec.Name, err)
	}
	return nil
}

// adoptMountBookkeeping settles the mount fields a migrated or imported disk
// brings with it. The guest's fstab travelled inside the disk, already written
// for virtiofs, so it must not be rewritten, and new tags must not reuse an old one.
func adoptMountBookkeeping(v *instance.VMSpec) {
	if len(v.Mounts) == 0 {
		return
	}
	v.MountFS = mountFSVirtiofs
	next := 0
	for _, m := range v.Mounts {
		var n int
		if _, err := fmt.Sscanf(m.Tag, "mount%d", &n); err == nil && n >= next {
			next = n + 1
		}
	}
	if next > v.NextMountIndex {
		v.NextMountIndex = next
	}
}
