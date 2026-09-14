// Package intent implements named groups of VM/container instances that
// share a network.
package intent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent/ipam"
	"github.com/anvil-project/anvil/internal/store"
)

// Store is the subset of *store.Store's methods Manager needs.
type Store interface {
	PutIntent(store.Intent) error
	GetIntentByID(id string) (store.Intent, error)
	GetIntentByName(name string) (store.Intent, error)
	ListIntents() ([]store.Intent, error)
	DeleteIntentByID(id string) error
}

// Instances is the subset of *instance.Manager's methods Manager needs.
type Instances interface {
	Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error
	GetByID(id string) (*instance.Spec, error)
	Delete(ctx context.Context, names []string, purge bool) error
}

// Networker creates and manages the shared bridge network for one intent.
// A nil Networker means intent members get no shared network.
type Networker interface {
	CreateNetwork(ctx context.Context, name, bridgeInterface, subnet, gateway, dockerIPRange string) error

	// RemoveNetwork deletes name's network. Removing an already-gone
	// network must not be an error.
	RemoveNetwork(ctx context.Context, name string) error

	// ContainerAddress returns containerID's assigned address on networkName.
	ContainerAddress(ctx context.Context, networkName, containerID string) (string, error)
}

type Manager struct {
	Store     Store
	Instances Instances
	Networker Networker

	// mu serializes Launch/Remove/Delete's read-modify-write sequence
	// against an intent's Store record.
	mu sync.Mutex
}

func NewManager(s Store, instances Instances, networker Networker) *Manager {
	return &Manager{Store: s, Instances: instances, Networker: networker}
}

// maxSubnetAttempts bounds how many times ensureNetwork retries with a
// different hashed subnet before giving up.
const maxSubnetAttempts = 5

// ensureNetwork creates the intent's shared bridge network if it doesn't
// have one yet, mutating it.Network in place. pinned, if set, overrides auto-allocation.
func (m *Manager) ensureNetwork(ctx context.Context, it *store.Intent, pinned *instance.PinnedNetwork) error {
	if it.Network != nil {
		if pinned != nil && (it.Network.Subnet != pinned.Subnet || it.Network.Gateway != pinned.Gateway) {
			// The existing network doesn't match the pinned network this
			// migrated member needs.
			return fmt.Errorf("intent: %q already has a network (%s) that doesn't match the pinned one (%s) this migrated member needs",
				it.Name, it.Network.Subnet, pinned.Subnet)
		}
		return nil
	}
	if m.Networker == nil {
		return nil
	}

	engineName := "anvil-" + it.ID
	// Uses the last 10 characters of the ULID to stay within Linux's
	// interface name length limit while keeping the random portion.
	bridgeIface := "anvil" + it.ID[len(it.ID)-10:]

	if pinned != nil {
		if err := m.Networker.CreateNetwork(ctx, engineName, bridgeIface, pinned.Subnet, pinned.Gateway, pinned.DockerIPRange); err != nil {
			return fmt.Errorf("intent: creating %q's pinned network: %w", it.Name, err)
		}
		it.Network = &store.IntentNetwork{
			EngineNetworkName: engineName,
			BridgeInterface:   bridgeIface,
			Subnet:            pinned.Subnet,
			Gateway:           pinned.Gateway,
			DockerIPRange:     pinned.DockerIPRange,
		}
		return nil
	}

	var lastErr error
	for attempt := 0; attempt < maxSubnetAttempts; attempt++ {
		subnet, gateway, dockerIPRange := ipam.AllocateSubnet(it.ID, attempt)
		if err := m.Networker.CreateNetwork(ctx, engineName, bridgeIface, subnet, gateway, dockerIPRange); err != nil {
			lastErr = err
			continue
		}
		it.Network = &store.IntentNetwork{
			EngineNetworkName: engineName,
			BridgeInterface:   bridgeIface,
			Subnet:            subnet,
			Gateway:           gateway,
			DockerIPRange:     dockerIPRange,
		}
		return nil
	}
	return fmt.Errorf("intent: creating a network for %q after %d attempts: %w", it.Name, maxSubnetAttempts, lastErr)
}

// nextVMAddress picks the lowest free static address in subnet's reserved
// low range, skipping addresses already used by members.
func nextVMAddress(subnet string, members []store.IntentMember) (string, error) {
	ips := make([]string, 0, len(members))
	for _, mem := range members {
		if mem.Kind == instance.KindVM && mem.IP != "" {
			ips = append(ips, mem.IP)
		}
	}
	used, err := ipam.UsedVMIndices(subnet, ips)
	if err != nil {
		return "", err
	}
	idx, err := ipam.NextFreeVMIndex(used)
	if err != nil {
		return "", err
	}
	return ipam.VMAddress(subnet, idx)
}

// hostsFor builds a role -> IP map from every member with a recorded
// address, for a new VM member's ExtraHosts.
func hostsFor(members []store.IntentMember) map[string]string {
	hosts := make(map[string]string, len(members))
	for _, mem := range members {
		if mem.IP != "" {
			hosts[mem.Role] = mem.IP
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return hosts
}

// vmHostsFor is hostsFor, restricted to VM members, for a new container
// member's ExtraHosts.
func vmHostsFor(members []store.IntentMember) map[string]string {
	hosts := make(map[string]string, len(members))
	for _, mem := range members {
		if mem.Kind == instance.KindVM && mem.IP != "" {
			hosts[mem.Role] = mem.IP
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return hosts
}

// stripCIDR returns just the address portion of a "10.55.201.4/24"-shaped
// string.
func stripCIDR(s string) string {
	if idx := strings.Index(s, "/"); idx != -1 {
		return s[:idx]
	}
	return s
}

// Launch provisions one intent member, creating the intent if it doesn't
// exist, and wires it onto the shared network if one is configured.
func (m *Manager) Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	if params.IntentName == "" {
		return fmt.Errorf("intent: Launch called without an intent name")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	it, err := m.Store.GetIntentByName(params.IntentName)
	if err != nil {
		it = store.Intent{ID: ulid.Make().String(), Name: params.IntentName}
	}

	if err := m.ensureNetwork(ctx, &it, params.PinnedNetwork); err != nil {
		progress(instance.LaunchEvent{Err: err})
		return err
	}

	role := params.Role
	if role == "" {
		role = params.Name
	}

	labels := make(map[string]string, len(params.Labels)+2)
	for k, v := range params.Labels {
		labels[k] = v
	}
	labels["intent"] = it.Name
	labels["role"] = role
	launchParams := params
	launchParams.Labels = labels

	if it.Network != nil {
		switch {
		case launchParams.Container != nil:
			launchParams.Container.NetworkMode = it.Network.EngineNetworkName
			launchParams.Container.NetworkAlias = role
			launchParams.Container.ExtraHosts = vmHostsFor(it.Members)
		case launchParams.VM != nil:
			// A migrated member reuses its original address instead of
			// getting the next one in sequence.
			ip := params.PinnedStaticIP
			if ip == "" {
				var ipErr error
				ip, ipErr = nextVMAddress(it.Network.Subnet, it.Members)
				if ipErr != nil {
					progress(instance.LaunchEvent{Err: ipErr})
					return ipErr
				}
			}
			launchParams.VM.NetworkMode = "bridge"
			launchParams.VM.BridgeInterface = it.Network.BridgeInterface
			launchParams.VM.StaticIP = ip
			launchParams.VM.Gateway = it.Network.Gateway
			launchParams.VM.ExtraHosts = hostsFor(it.Members)
		}
	}

	var launched *instance.Spec
	var launchErr error
	err = m.Instances.Launch(ctx, launchParams, func(ev instance.LaunchEvent) {
		progress(ev)
		if ev.Instance != nil {
			launched = ev.Instance
		}
		if ev.Err != nil {
			launchErr = ev.Err
		}
	})
	if err != nil {
		return err
	}
	if launchErr != nil {
		return launchErr
	}

	// Recorded so a later member can resolve this one by name; a failure
	// to read it back is logged but not fatal.
	memberIP := ""
	if it.Network != nil {
		switch {
		case launched.VM != nil:
			memberIP = stripCIDR(launched.VM.StaticIP)
		case launched.Container != nil:
			ip, err := m.Networker.ContainerAddress(ctx, it.Network.EngineNetworkName, launched.Container.ContainerID)
			if err != nil {
				log.Printf("intent: reading %s's address on %q: %v", launched.Name, it.Network.EngineNetworkName, err)
			} else {
				memberIP = ip
			}
		}
	}

	it.Members = append(it.Members, store.IntentMember{InstanceID: launched.ID, Role: role, Kind: launched.Kind, IP: memberIP})
	return m.Store.PutIntent(it)
}

func (m *Manager) List() ([]store.Intent, error) {
	return m.Store.ListIntents()
}

func (m *Manager) Info(name string) (store.Intent, error) {
	return m.Store.GetIntentByName(name)
}

// Remove ungroups member (matched by instance ID or role) from name
// without deleting the underlying instance.
func (m *Manager) Remove(name, member string) (store.Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	it, err := m.Store.GetIntentByName(name)
	if err != nil {
		return store.Intent{}, err
	}

	kept := it.Members[:0]
	found := false
	for _, mem := range it.Members {
		if mem.InstanceID == member || mem.Role == member {
			found = true
			continue
		}
		kept = append(kept, mem)
	}
	if !found {
		return store.Intent{}, fmt.Errorf("intent: %q has no member %q", name, member)
	}
	it.Members = kept

	if err := m.Store.PutIntent(it); err != nil {
		return store.Intent{}, err
	}
	return it, nil
}

// Delete removes the intent record and its shared network (best-effort).
// If purgeMembers, every member instance is deleted too.
func (m *Manager) Delete(ctx context.Context, name string, purgeMembers bool) (store.Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	it, err := m.Store.GetIntentByName(name)
	if err != nil {
		return store.Intent{}, err
	}

	if purgeMembers && len(it.Members) > 0 {
		var names []string
		for _, mem := range it.Members {
			spec, err := m.Instances.GetByID(mem.InstanceID)
			if err != nil {
				continue // already gone, nothing to purge
			}
			names = append(names, spec.Name)
		}
		if len(names) > 0 {
			if err := m.Instances.Delete(ctx, names, true); err != nil {
				return store.Intent{}, fmt.Errorf("intent: purging members of %q: %w", name, err)
			}
		}
	}

	if m.Networker != nil && it.Network != nil {
		if err := m.Networker.RemoveNetwork(ctx, it.Network.EngineNetworkName); err != nil {
			log.Printf("intent: removing network for %q: %v", name, err)
		}
	}

	if err := m.Store.DeleteIntentByID(it.ID); err != nil {
		return store.Intent{}, err
	}
	return it, nil
}
