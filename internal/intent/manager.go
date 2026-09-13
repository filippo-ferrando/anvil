// Package intent implements named groups of VM/container instances (M4).
// There's deliberately no separate "create a member" codepath here: a
// member is provisioned through the exact same instance.Manager.Launch
// any standalone `anvil launch` uses (same image resolution, cloud-init,
// container pulls, progress reporting), just additionally tagged,
// networked, and tracked. internal/daemon routes a LaunchRequest through
// Manager.Launch (below) instead of straight to instance.Manager.Launch
// only when the request's intent_name is set — see
// internal/daemon/server.go.
package intent

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent/ipam"
	"github.com/anvil-project/anvil/internal/store"
)

// Store is the subset of *store.Store's methods Manager needs — same
// narrow-interface pattern as internal/vm.Backend's Source and
// internal/container.DockerBackend's Source.
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

// Networker creates the shared bridge network for one intent — engine-
// specific (a Docker implementation exists, see
// internal/container.DockerNetworker; a Podman one would satisfy the same
// interface once that backend lands). CreateNetwork is expected to
// actually create the network with exactly the parameters given, no
// negotiation — retrying with a different subnet on a collision is
// Manager.ensureNetwork's job, not the Networker's.
//
// A nil Networker is fine: Manager just never gives intent members a
// shared network then (today's pre-M4-networking behavior), same fallback
// pattern as internal/container.Backend.Podman staying nil.
type Networker interface {
	CreateNetwork(ctx context.Context, name, bridgeInterface, subnet, gateway, dockerIPRange string) error

	// RemoveNetwork deletes name's network — called by Manager.Delete once
	// an intent itself is being deleted (see there for why this is
	// best-effort). Removing an already-gone network must not be an
	// error, matching CreateNetwork's own idempotency expectations.
	RemoveNetwork(ctx context.Context, name string) error

	// ContainerAddress returns containerID's assigned address on
	// networkName — called right after a container member is created, so
	// its IP can be recorded (store.IntentMember.IP) for a later-launched
	// VM member's static hosts entries (see instance.VMSpec.ExtraHosts).
	// Not meaningful for Podman/other engines the same way; VM addresses
	// don't need this, they're assigned by anvil itself up front.
	ContainerAddress(ctx context.Context, networkName, containerID string) (string, error)
}

type Manager struct {
	Store     Store
	Instances Instances
	Networker Networker
}

func NewManager(s Store, instances Instances, networker Networker) *Manager {
	return &Manager{Store: s, Instances: instances, Networker: networker}
}

// maxSubnetAttempts bounds how many times ensureNetwork retries with a
// different hashed subnet before giving up — a real (if rare) case, since
// subnets aren't coordinated against anything already on the host, just
// deterministically hashed from the intent's ID.
const maxSubnetAttempts = 5

// ensureNetwork creates it's shared bridge network if it doesn't have one
// yet, mutating it.Network in place on success. A no-op if there's no
// Networker configured, or the intent already has a network (the normal
// case for every member after the first).
//
// pinned, when non-nil (only ever set by `anvil migrate-import`
// relaunching a migrated intent's member, see LaunchParams.PinnedNetwork's
// doc comment), overrides the normal hashed-subnet auto-allocation below
// with an exact subnet/gateway/docker-IP-range instead — no retry loop,
// unlike the auto-allocated path, since there's no fallback subnet to
// try that would still mean the same thing: if the target can't create a
// network with this exact subnet (e.g. it collides with something else
// already there), that's a real migration failure to report, not
// something to silently paper over with a different subnet the source's
// already-baked-in guest network config knows nothing about.
func (m *Manager) ensureNetwork(ctx context.Context, it *store.Intent, pinned *instance.PinnedNetwork) error {
	if m.Networker == nil || it.Network != nil {
		return nil
	}

	engineName := "anvil-" + it.ID
	// "anvil" (5 chars) + the first 10 of a 26-char ULID = 15, right at
	// Linux's 15-usable-character interface name limit (IFNAMSIZ=16
	// including the trailing NUL) — see internal/vm/network.TapName for
	// the same constraint on the VM side.
	bridgeIface := "anvil" + it.ID[:10]

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

func countVMMembers(members []store.IntentMember) int {
	n := 0
	for _, mem := range members {
		if mem.Kind == instance.KindVM {
			n++
		}
	}
	return n
}

// hostsFor builds a role -> IP map from every already-known member with a
// recorded address, for a new VM member's ExtraHosts — a VM has no other
// way to resolve any peer by name (unlike a container, which gets
// Docker's embedded DNS for its container peers for free), so it needs an
// entry for every kind of member, not just VMs.
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

// vmHostsFor is hostsFor, restricted to VM members — for a new container
// member's ExtraHosts. A container already resolves other container peers
// via Docker's own embedded DNS (see NetworkAlias), so only VM members
// (invisible to Docker's DNS) need a static entry here.
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
// string, for recording in store.IntentMember.IP (which other members'
// hosts entries reference directly, with no CIDR suffix).
func stripCIDR(s string) string {
	if idx := strings.Index(s, "/"); idx != -1 {
		return s[:idx]
	}
	return s
}

// Launch provisions one intent member: params.IntentName is resolved to an
// existing intent, or a new one is created on the fly if no intent by
// that name exists yet (there's no separate "create an intent" step —
// the first member launched under a given intent_name is what brings it
// into existence). params.Role labels the member, defaulting to the
// instance's own name if left blank.
//
// If a Networker is configured, the intent's shared network is created on
// demand (once, lazily, on this call if it doesn't exist yet) and the
// member is wired onto it before Instances.Launch runs: a container gets
// its NetworkMode set to the engine network's name (Docker/Podman then
// handle its address assignment themselves); a VM gets NetworkMode
// "bridge" plus a statically-assigned address in the network's reserved
// low range (see internal/intent/ipam), consumed by internal/vm.Backend
// to attach a tap device and write a matching cloud-init network-config.
//
// progress is instance.Manager.Launch's own progress callback, unchanged
// — the caller (internal/daemon) sees exactly the same event stream a
// standalone launch would produce.
//
// Callers must not call this with params.IntentName == "" — that's a
// standalone launch, and belongs on instance.Manager.Launch directly, not
// here.
func (m *Manager) Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	if params.IntentName == "" {
		return fmt.Errorf("intent: Launch called without an intent name")
	}

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
	// The intent's name, not its ID — a name is what's actually useful to
	// show back on `anvil info` (see internal/cli/commands/info.go), and
	// names are already how intents get looked up (GetIntentByName), so
	// there's no real reason to prefer the ID here.
	labels["intent"] = it.Name
	labels["role"] = role
	launchParams := params
	launchParams.Labels = labels

	if it.Network != nil {
		switch {
		case launchParams.Container != nil:
			launchParams.Container.NetworkMode = it.Network.EngineNetworkName
			// NetworkAlias lets Docker's embedded DNS resolve this
			// container by role for other container peers; ExtraHosts
			// covers what that DNS can't (VM peers) — see hostsFor/
			// vmHostsFor's doc comments for why the two aren't symmetric.
			launchParams.Container.NetworkAlias = role
			launchParams.Container.ExtraHosts = vmHostsFor(it.Members)
		case launchParams.VM != nil:
			// A migrated member (see LaunchParams.PinnedStaticIP's doc
			// comment) reuses its own exact original address instead of
			// getting the next one in sequence here — its guest's
			// already-baked-in static network config has no way to
			// learn a new one, since a migrated disk skips cloud-init
			// entirely on relaunch.
			ip := params.PinnedStaticIP
			if ip == "" {
				var ipErr error
				ip, ipErr = ipam.VMAddress(it.Network.Subnet, countVMMembers(it.Members))
				if ipErr != nil {
					progress(instance.LaunchEvent{Err: ipErr})
					return ipErr
				}
			}
			launchParams.VM.NetworkMode = "bridge"
			launchParams.VM.BridgeInterface = it.Network.BridgeInterface
			launchParams.VM.StaticIP = ip
			launchParams.VM.Gateway = it.Network.Gateway
			// A VM has no DNS resolution mechanism of its own, so it needs
			// a static entry for every already-known member, not just VMs.
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

	// Recorded so a member launched *after* this one can resolve it by
	// name (see hostsFor/vmHostsFor above) — a VM's address was already
	// decided before Launch ran; a container's is assigned by Docker
	// itself, so it has to be read back now. A failure to read it back
	// doesn't undo the (already successful) launch, it just means later
	// members won't get a hosts entry for this one — logged, not fatal.
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
// without deleting the underlying instance — delete it separately via
// instance.Manager.Delete for that.
func (m *Manager) Remove(name, member string) (store.Intent, error) {
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

// Delete removes the intent record and its shared network (if it has
// one). If purgeMembers, every member instance is deleted outright too
// (force teardown, matching instance.Manager.Delete's own semantics)
// before the network is torn down, so the common case is a clean
// removal; otherwise members are left running, just ungrouped, and
// removing the network is attempted anyway but best-effort — a container
// member still attached to it, or a VM member's tap device still on its
// bridge, will likely make the engine refuse to remove it, and that's not
// treated as a failure of Delete itself (the intent record is what
// actually matters here, a leftover network is just a cleanup nice-to-have).
func (m *Manager) Delete(ctx context.Context, name string, purgeMembers bool) (store.Intent, error) {
	it, err := m.Store.GetIntentByName(name)
	if err != nil {
		return store.Intent{}, err
	}

	if purgeMembers && len(it.Members) > 0 {
		var names []string
		for _, mem := range it.Members {
			spec, err := m.Instances.GetByID(mem.InstanceID)
			if err != nil {
				continue // already gone somehow, nothing to purge
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
