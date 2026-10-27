//go:build linux

package vm

import (
	"context"
	"fmt"
	"net/netip"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostpath"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/ipam"
)

var _ instance.Mounter = (*FCBackend)(nil)

// nfsFstabLine is m's fstab entry in a Firecracker guest: NFSv3 from anvild on the gateway.
// addr= and the fixed ports let the kernel mount it without mount.nfs, rpcbind or locking.
func nfsFstabLine(gateway string, m instance.Mount) string {
	opts := fmt.Sprintf("vers=3,proto=tcp,port=%[1]d,mountport=%[1]d,mountproto=tcp,nolock,addr=%[2]s,mountaddr=%[2]s,_netdev,nofail",
		fcNFSPort, gateway)
	if m.ReadOnly {
		opts += ",ro"
	}
	return fmt.Sprintf("%s:/%s %s nfs %s 0 0", gateway, m.Tag, fstabEscape(m.GuestPath), opts)
}

// guestAddrs returns spec's own address and its gateway, where its mounts are served.
func guestAddrs(v *instance.VMSpec) (guest, gateway netip.Addr, err error) {
	if guest, err = netip.ParseAddr(ipam.StripCIDR(v.StaticIP)); err != nil {
		return guest, gateway, fmt.Errorf("vm: no usable guest address %q: %w", v.StaticIP, err)
	}
	if gateway, err = netip.ParseAddr(v.Gateway); err != nil {
		return guest, gateway, fmt.Errorf("vm: no usable gateway %q: %w", v.Gateway, err)
	}
	return guest, gateway, nil
}

// exportMounts serves all of spec's mounts to its guest address, ahead of the guest booting.
func (b *FCBackend) exportMounts(spec *instance.Spec) error {
	v := spec.VM
	if len(v.Mounts) == 0 {
		return nil
	}
	guest, gateway, err := guestAddrs(v)
	if err != nil {
		return err
	}
	for _, m := range v.Mounts {
		if err := hostpath.CheckResolved(m.HostPath); err != nil {
			b.nfs.forget(guest)
			return fmt.Errorf("vm: shared folder %w", err)
		}
		if err := b.nfs.export(gateway, guest, m.Tag, m.HostPath, m.ReadOnly); err != nil {
			b.nfs.forget(guest)
			return err
		}
	}
	b.mu.Lock()
	b.exported[spec.ID] = guest
	b.mu.Unlock()
	return nil
}

// unexportMounts stops serving anything to instanceID's guest.
func (b *FCBackend) unexportMounts(instanceID string) {
	b.mu.Lock()
	guest, ok := b.exported[instanceID]
	delete(b.exported, instanceID)
	b.mu.Unlock()
	if ok {
		b.nfs.forget(guest)
	}
}

// Mount shares hostPath into the guest at guestPath over NFS. A running VM with a guest
// agent gets it live; otherwise the seed is rebuilt and a running VM restarts.
func (b *FCBackend) Mount(ctx context.Context, spec *instance.Spec, hostPath, guestPath string, readOnly bool) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Mount called with a nil VMSpec")
	}
	v := spec.VM
	if err := checkNewMount(spec, hostPath, guestPath, 0); err != nil {
		return err
	}
	m := instance.Mount{HostPath: hostPath, GuestPath: guestPath, Tag: fmt.Sprintf("mount%d", v.NextMountIndex), ReadOnly: readOnly}

	if b.liveMountReady(ctx, spec) {
		guest, gateway, err := guestAddrs(v)
		if err != nil {
			return err
		}
		if err := b.nfs.export(gateway, guest, m.Tag, m.HostPath, m.ReadOnly); err != nil {
			return err
		}
		b.mu.Lock()
		b.exported[spec.ID] = guest
		b.mu.Unlock()
		if _, err := b.guestExec(ctx, spec.ID, mountLiveScript(m, nfsFstabLine(v.Gateway, m))); err != nil {
			b.nfs.unexport(guest, m.Tag)
			return fmt.Errorf("vm: mounting %s in the guest: %w", m.GuestPath, err)
		}
		v.NextMountIndex++
		v.Mounts = append(v.Mounts, m)
		return nil
	}
	v.NextMountIndex++
	v.Mounts = append(v.Mounts, m)
	return reseedAndRestart(ctx, b, b.QEMU, spec)
}

// Umount removes a mount by its guest path, live when it can.
func (b *FCBackend) Umount(ctx context.Context, spec *instance.Spec, guestPath string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Umount called with a nil VMSpec")
	}
	v := spec.VM
	idx, err := mountIndex(spec, guestPath)
	if err != nil {
		return err
	}
	m := v.Mounts[idx]

	if b.liveMountReady(ctx, spec) {
		if _, err := b.guestExec(ctx, spec.ID, umountLiveScript(m)); err != nil {
			return fmt.Errorf("vm: unmounting %s in the guest (still in use?): %w", m.GuestPath, err)
		}
		if guest, _, err := guestAddrs(v); err == nil {
			b.nfs.unexport(guest, m.Tag)
		}
		v.Mounts = append(v.Mounts[:idx], v.Mounts[idx+1:]...)
		return nil
	}
	v.Mounts = append(v.Mounts[:idx], v.Mounts[idx+1:]...)
	return reseedAndRestart(ctx, b, b.QEMU, spec)
}

// liveMountReady reports whether a mount change can go to the running guest through its agent.
func (b *FCBackend) liveMountReady(ctx context.Context, spec *instance.Spec) bool {
	_, running := b.lookup(spec.ID)
	if !running {
		return false
	}
	if info, _ := b.GuestInfo(spec); !info.AgentConnected {
		return false
	}
	_, err := b.guestExec(ctx, spec.ID, "true")
	return err == nil // guest-exec can be blocked by the distro's agent policy
}
