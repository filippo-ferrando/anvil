//go:build linux

package vm

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

// checkNewPort rejects a port forward spec can't take: on a bridge, or one it already has.
func checkNewPort(spec *instance.Spec, port instance.PortMapping) (string, error) {
	if spec.VM.NetworkMode == "bridge" {
		return "", fmt.Errorf("vm: %s uses bridge networking; it has its own address, so no host-forwarded ports apply", spec.Name)
	}
	proto := instance.Protocol(port.Protocol)
	for _, p := range spec.VM.Ports {
		if p.HostPort == port.HostPort && instance.Protocol(p.Protocol) == proto {
			return "", fmt.Errorf("vm: %s already forwards host port %d/%s", spec.Name, port.HostPort, proto)
		}
	}
	return proto, nil
}

// portIndex returns the index of spec's port forward for hostPort/proto.
func portIndex(spec *instance.Spec, hostPort int, proto string) (int, error) {
	idx := slices.IndexFunc(spec.VM.Ports, func(p instance.PortMapping) bool {
		return p.HostPort == hostPort && instance.Protocol(p.Protocol) == proto
	})
	if idx == -1 {
		return -1, fmt.Errorf("vm: %s has no port forward for host port %d/%s (currently exposed: %s)",
			spec.Name, hostPort, proto, instance.FormatPorts(spec.VM.Ports))
	}
	return idx, nil
}

// AddPort adds a host-to-guest SLIRP port forward to spec, applied live
// over QMP (no restart, unlike Mount) if running, else persisted for Start.
func (b *Backend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: AddPort called with a nil VMSpec")
	}
	proto, err := checkNewPort(spec, port)
	if err != nil {
		return err
	}

	proc, running := b.lookup(spec.ID)
	if running && proc.QMP != nil {
		if err := proc.QMP.AddHostForward(ctx, qemu.NetdevID, qemu.HostForward{
			HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: proto,
		}); err != nil {
			return fmt.Errorf("vm: adding live port forward: %w", err)
		}
	}

	spec.VM.Ports = append(spec.VM.Ports, instance.PortMapping{HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: proto})
	return nil
}

// RemovePort removes a port forward previously added with AddPort or at
// launch, identified by hostPort/protocol.
func (b *Backend) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: RemovePort called with a nil VMSpec")
	}
	proto := instance.Protocol(protocol)
	idx, err := portIndex(spec, hostPort, proto)
	if err != nil {
		return err
	}

	proc, running := b.lookup(spec.ID)
	if running && proc.QMP != nil {
		if err := proc.QMP.RemoveHostForward(ctx, qemu.NetdevID, hostPort, proto); err != nil {
			return fmt.Errorf("vm: removing live port forward: %w", err)
		}
	}

	spec.VM.Ports = append(spec.VM.Ports[:idx], spec.VM.Ports[idx+1:]...)
	return nil
}

// recentPorts remembers ports allocateFreePort handed out lately, so VMs started
// in parallel never get the same port before QEMU has bound it.
var (
	recentPortsMu sync.Mutex
	recentPorts   = map[int]time.Time{}
)

const recentPortTTL = time.Minute

// allocateFreePort asks the OS for an ephemeral port by briefly binding
// to port 0, reading back what was assigned, then releasing it.
func allocateFreePort() (int, error) {
	recentPortsMu.Lock()
	defer recentPortsMu.Unlock()
	now := time.Now()
	for p, at := range recentPorts {
		if now.Sub(at) > recentPortTTL {
			delete(recentPorts, p)
		}
	}
	for range 16 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if _, taken := recentPorts[port]; !taken {
			recentPorts[port] = now
			return port, nil
		}
	}
	return 0, fmt.Errorf("vm: no free port found")
}
