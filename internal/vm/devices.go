//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"maps"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

var _ instance.DeviceHolder = (*Backend)(nil)

// DeviceClaimer hands host devices to anvild's user and back; hostdev.Client talks to the root helper.
type DeviceClaimer interface {
	Claim(ctx context.Context, bus, addr string) (hostdev.Claim, error)
	Release(ctx context.Context, cl hostdev.Claim) error
}

// resolveDevice is hostdev.Resolve and hostdev.Members, swappable for tests.
var resolveDevice = func(id string) (bus, addr string, members []string, err error) {
	bus, addr, err = hostdev.Resolve(id)
	if err != nil {
		return "", "", nil, err
	}
	return bus, addr, hostdev.Members(bus, addr), nil
}

// releaseTimeout bounds giving devices back once their VM is gone.
const releaseTimeout = 30 * time.Second

// HeldDevices returns which instance holds each claimed device, by sysfs name.
func (b *Backend) HeldDevices() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.held)
}

// claimDevices reserves and claims every device in spec, adding them to cfg. On error
// nothing stays claimed or reserved.
func (b *Backend) claimDevices(ctx context.Context, spec *instance.Spec, cfg *qemu.Config) ([]hostdev.Claim, error) {
	if len(spec.VM.Devices) == 0 {
		return nil, nil
	}
	type target struct{ id, bus, addr string }
	var targets []target
	var reserve []string
	for _, id := range spec.VM.Devices {
		bus, addr, members, err := resolveDevice(id)
		if err != nil {
			return nil, fmt.Errorf("vm: %w", err)
		}
		targets = append(targets, target{id, bus, addr})
		reserve = append(reserve, members...)
	}

	b.mu.Lock()
	for _, addr := range reserve {
		if owner, ok := b.held[addr]; ok && owner != spec.ID {
			b.mu.Unlock()
			return nil, fmt.Errorf("vm: device %s is used by another running instance (%s)", addr, owner)
		}
	}
	for _, addr := range reserve {
		b.held[addr] = spec.ID
	}
	b.mu.Unlock()

	var claims []hostdev.Claim
	for _, t := range targets {
		cl, err := b.Devices.Claim(ctx, t.bus, t.addr)
		if err != nil {
			b.releaseDevices(spec.ID, claims)
			b.unhold(spec.ID)
			return nil, fmt.Errorf("vm: claiming %s: %w", t.id, err)
		}
		claims = append(claims, cl)
		for _, m := range cl.Members {
			if t.bus == "usb" {
				cfg.USBDevices = append(cfg.USBDevices, m.Addr)
			} else {
				cfg.PCIDevices = append(cfg.PCIDevices, m.Addr)
			}
		}
	}
	return claims, nil
}

// releaseDevices gives claims back to the host, logging what fails: the VM is gone either way.
func (b *Backend) releaseDevices(instanceID string, claims []hostdev.Claim) {
	if len(claims) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	for _, cl := range claims {
		if err := b.Devices.Release(ctx, cl); err != nil {
			log.Printf("vm: giving %s:%s back to the host after %s: %v", cl.Bus, cl.Addr, instanceID, err)
		}
	}
}

func (b *Backend) unhold(instanceID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for addr, owner := range b.held {
		if owner == instanceID {
			delete(b.held, addr)
		}
	}
}

// holdClaims marks a reconciled VM's devices as held again.
func (b *Backend) holdClaims(instanceID string, claims []hostdev.Claim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cl := range claims {
		for _, m := range cl.Members {
			b.held[m.Addr] = instanceID
		}
	}
}

// releaseRuntimeDevices gives back what the runtime state of instanceID says it claimed.
func (b *Backend) releaseRuntimeDevices(instanceID string) {
	rt, found, err := loadRuntimeState(instanceDir(instanceID))
	if err == nil && found {
		b.releaseDevices(instanceID, rt.Devices)
	}
	b.unhold(instanceID)
}
