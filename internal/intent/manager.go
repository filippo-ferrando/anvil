// Package intent implements named groups of VM/container instances that
// share a network.
package intent

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/ipam"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
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
	List(kind instance.Kind) ([]*instance.Spec, error)
	Delete(ctx context.Context, names []string) error
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

	// ContainerAddresses returns every container's address on networkName, keyed by container ID.
	ContainerAddresses(ctx context.Context, networkName string) (map[string]string, error)

	// ListNetworks returns the names of every network the engine
	// currently has, used by ReconcileNetworks to find orphans.
	ListNetworks(ctx context.Context) ([]string, error)
}

// networkNamePrefix is what ensureNetwork names every network it creates
// ("anvil-" + the owning intent's ID); ReconcileNetworks uses it to recognize anvil's own networks.
const networkNamePrefix = "anvil-"

// DNS is told to reload its zones whenever an intent's members or
// network change. A nil DNS means members only get static hosts entries.
type DNS interface {
	Refresh(ctx context.Context) error
}

type Manager struct {
	Store     Store
	Instances Instances
	Networker Networker
	DNS       DNS

	// locks serializes Launch/Remove/Delete's read-modify-write of one intent's
	// Store record, so a slow launch only blocks its own intent.
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	// pending holds intents whose member is still launching, by name, so a new
	// intent's zone is served before its first member boots.
	pendingMu sync.Mutex
	pending   map[string]store.Intent
}

// lock takes name's intent lock and returns its release.
// ponytail: entries are never freed, one small mutex per intent name ever used.
func (m *Manager) lock(name string) func() {
	m.locksMu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	mu, ok := m.locks[name]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[name] = mu
	}
	m.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
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
			return fmt.Errorf("intent: %q already has a network (%s) that doesn't match the pinned one (%s) this migrated member needs",
				it.Name, it.Network.Subnet, pinned.Subnet)
		}
		return m.recreateIfGone(ctx, it.Network)
	}
	if m.Networker == nil {
		return nil
	}

	engineName := networkNamePrefix + it.ID
	bridgeIface := bridgeFor(it.ID)

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

// bridgeFor names an intent's bridge from its ID: the last 10 characters of the ULID
// stay within Linux's interface name length limit while keeping the random portion.
func bridgeFor(intentID string) string {
	if len(intentID) < 10 {
		return "anvil" + intentID
	}
	return "anvil" + intentID[len(intentID)-10:]
}

// recreateIfGone brings back n with its recorded addresses when the engine lost it
// (e.g. `docker network prune` on an intent with only VMs).
func (m *Manager) recreateIfGone(ctx context.Context, n *store.IntentNetwork) error {
	if m.Networker == nil {
		return nil
	}
	names, err := m.Networker.ListNetworks(ctx)
	if err != nil {
		return fmt.Errorf("intent: listing networks: %w", err)
	}
	for _, name := range names {
		if name == n.EngineNetworkName {
			return nil
		}
	}
	log.Printf("intent: network %q is gone, creating it again", n.EngineNetworkName)
	return m.Networker.CreateNetwork(ctx, n.EngineNetworkName, n.BridgeInterface, n.Subnet, n.Gateway, n.DockerIPRange)
}

// networkUsers returns the instances still attached to n, whose traffic would stop if it
// were removed. A listing error counts as in use, so nothing is cut off by mistake.
func (m *Manager) networkUsers(n *store.IntentNetwork) []string {
	specs, err := m.Instances.List("")
	if err != nil {
		log.Printf("intent: listing instances using %q: %v", n.EngineNetworkName, err)
		return []string{"(unknown)"}
	}
	var users []string
	for _, spec := range specs {
		if (spec.VM != nil && spec.VM.BridgeInterface == n.BridgeInterface) ||
			(spec.Container != nil && spec.Container.NetworkMode == n.EngineNetworkName) {
			users = append(users, spec.Name)
		}
	}
	return users
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

// hostsFor builds a role -> IP map from every member with a recorded address, for a new
// member's ExtraHosts. vmOnly keeps VM members only, for a new container member.
func hostsFor(members []store.IntentMember, vmOnly bool) map[string]string {
	hosts := make(map[string]string, len(members))
	for _, mem := range members {
		if mem.IP != "" && (!vmOnly || mem.Kind == instance.KindVM) {
			hosts[mem.Role] = mem.IP
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return hosts
}

// Launch provisions one intent member, creating the intent if it doesn't
// exist, and wires it onto the shared network if one is configured.
func (m *Manager) Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	if params.IntentName == "" {
		return fmt.Errorf("intent: Launch called without an intent name")
	}

	defer m.lock(params.IntentName)()

	it, err := m.Store.GetIntentByName(params.IntentName)
	if err != nil {
		it = store.Intent{ID: ulid.Make().String(), Name: params.IntentName}
	}

	role := params.Role
	if role == "" {
		role = params.Name
	}
	for _, mem := range it.Members {
		if mem.Role == role {
			err := fmt.Errorf("intent: %q already has a member with role %q", it.Name, role)
			progress(instance.LaunchEvent{Err: err})
			return err
		}
	}

	hadNetwork := it.Network != nil
	if err := m.ensureNetwork(ctx, &it, params.PinnedNetwork); err != nil {
		progress(instance.LaunchEvent{Err: err})
		return err
	}
	// A network made by this call is only referenced once the member is saved below.
	// Take it back out on any failure, or it survives until the next daemon restart reconciles.
	persisted := false
	if !hadNetwork && it.Network != nil && m.Networker != nil {
		defer func() {
			if persisted {
				return
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if err := m.Networker.RemoveNetwork(ctx, it.Network.EngineNetworkName); err != nil {
				log.Printf("intent: removing %q's unused network: %v", it.Name, err)
			}
		}()
	}
	m.setPending(&it)
	defer m.clearPending(it.Name)
	m.refreshDNS(ctx)

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
			launchParams.Container.ExtraHosts = hostsFor(it.Members, true)
			if m.DNS != nil {
				launchParams.Container.DNSServers = []string{it.Network.Gateway}
				launchParams.Container.DNSSearch = []string{Domain(it.Name)}
			}
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
			launchParams.VM.ExtraHosts = hostsFor(it.Members, false)
			if m.DNS != nil {
				launchParams.VM.DNSServers = []string{it.Network.Gateway}
				launchParams.VM.DNSSearch = []string{Domain(it.Name)}
			}
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
			memberIP = ipam.StripCIDR(launched.VM.StaticIP)
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
	if err := m.Store.PutIntent(it); err != nil {
		return err
	}
	persisted = true
	m.clearPending(it.Name)
	m.refreshDNS(ctx)
	return nil
}

func (m *Manager) List() ([]store.Intent, error) {
	return m.Store.ListIntents()
}

func (m *Manager) Info(name string) (store.Intent, error) {
	return m.Store.GetIntentByName(name)
}

// Remove ungroups member (by ID or role) from name without deleting the instance. If that leaves
// no members, the shared network is torn down too, best-effort, since members are often deleted individually.
func (m *Manager) Remove(ctx context.Context, name, member string) (store.Intent, error) {
	defer m.lock(name)()

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

	if len(it.Members) == 0 && m.Networker != nil && it.Network != nil {
		if users := m.networkUsers(it.Network); len(users) > 0 {
			// Kept on the record, so it's removed once they're gone and the intent is deleted.
			log.Printf("intent: keeping now-empty %q's network, still used by %s", name, strings.Join(users, ", "))
		} else if err := m.Networker.RemoveNetwork(ctx, it.Network.EngineNetworkName); err != nil {
			// Left set (not cleared) so ReconcileNetworks can still catch
			// and finish this later instead of losing track of it.
			log.Printf("intent: removing now-empty %q's network: %v", name, err)
		} else {
			it.Network = nil
		}
	}

	if err := m.Store.PutIntent(it); err != nil {
		return store.Intent{}, err
	}
	m.refreshDNS(ctx)
	return it, nil
}

// Delete removes the intent record and its shared network (best-effort).
// If purgeMembers, every member instance is deleted too.
func (m *Manager) Delete(ctx context.Context, name string, purgeMembers bool) (store.Intent, error) {
	defer m.lock(name)()

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
			if err := m.Instances.Delete(ctx, names); err != nil {
				return store.Intent{}, fmt.Errorf("intent: purging members of %q: %w", name, err)
			}
		}
	}

	if m.Networker != nil && it.Network != nil {
		if users := m.networkUsers(it.Network); len(users) > 0 {
			// ReconcileNetworks removes it at a later start, once nothing uses it.
			log.Printf("intent: keeping %q's network, still used by %s", name, strings.Join(users, ", "))
		} else if err := m.Networker.RemoveNetwork(ctx, it.Network.EngineNetworkName); err != nil {
			log.Printf("intent: removing network for %q: %v", name, err)
		}
	}

	if err := m.Store.DeleteIntentByID(it.ID); err != nil {
		return store.Intent{}, err
	}
	m.refreshDNS(ctx)
	return it, nil
}

// ReconcileNetworks removes any engine network that looks anvil-created (see networkNamePrefix) but
// isn't referenced by any intent in the store. Call once at daemon startup to recover from drift.
func (m *Manager) ReconcileNetworks(ctx context.Context) error {
	if m.Networker == nil {
		return nil
	}
	intents, err := m.Store.ListIntents()
	if err != nil {
		return fmt.Errorf("intent: listing intents to reconcile networks: %w", err)
	}
	known := make(map[string]bool, len(intents))
	for _, it := range intents {
		if it.Network != nil {
			known[it.Network.EngineNetworkName] = true
		}
	}

	names, err := m.Networker.ListNetworks(ctx)
	if err != nil {
		return fmt.Errorf("intent: listing engine networks to reconcile: %w", err)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, networkNamePrefix) || known[name] {
			continue
		}
		// An instance left on a deleted intent's network keeps it until it's gone.
		if users := m.networkUsers(&store.IntentNetwork{EngineNetworkName: name, BridgeInterface: bridgeFor(strings.TrimPrefix(name, networkNamePrefix))}); len(users) > 0 {
			continue
		}
		log.Printf("intent: removing orphaned network %q (no intent references it)", name)
		if err := m.Networker.RemoveNetwork(ctx, name); err != nil {
			log.Printf("intent: removing orphaned network %q: %v", name, err)
		}
	}
	return nil
}

// Domain returns the DNS zone an intent's members live in, e.g. "myapp.anvil".
func Domain(intentName string) string {
	return dns.Domain(intentName)
}

// MemberName is the instance name a member gets when none is given, e.g. "shop-db".
func MemberName(intentName, role string) string {
	return intentName + "-" + role
}

// MemberDNSName returns the fully qualified name a member resolves as,
// e.g. "db.myapp.anvil".
func MemberDNSName(intentName, role string) string {
	return dns.Label(role) + "." + Domain(intentName)
}

func (m *Manager) setPending(it *store.Intent) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if m.pending == nil {
		m.pending = make(map[string]store.Intent)
	}
	m.pending[it.Name] = *it
}

func (m *Manager) clearPending(name string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	delete(m.pending, name)
}

// refreshDNS reloads the DNS server's zones; a failure only means a
// zone is served late, so it is logged, not returned.
func (m *Manager) refreshDNS(ctx context.Context) {
	if m.DNS == nil {
		return
	}
	if err := m.DNS.Refresh(ctx); err != nil {
		log.Printf("intent: refreshing DNS: %v", err)
	}
}

// containerLookupTimeout bounds each intent's live address lookup in DNSZones.
const containerLookupTimeout = 2 * time.Second

// DNSZones returns one zone per intent with a network, for dns.Server.
// Container addresses are re-read from the engine, since a restart can change them.
func (m *Manager) DNSZones(ctx context.Context) ([]dns.Zone, error) {
	intents, err := m.Store.ListIntents()
	if err != nil {
		return nil, err
	}
	stored := make(map[string]bool, len(intents))
	for _, it := range intents {
		stored[it.ID] = true
	}
	m.pendingMu.Lock()
	for _, p := range m.pending {
		if !stored[p.ID] {
			intents = append(intents, p)
		}
	}
	m.pendingMu.Unlock()

	var zones []dns.Zone
	for _, it := range intents {
		if z, ok := m.zoneFor(ctx, it); ok {
			zones = append(zones, z)
		}
	}
	return zones, nil
}

type nameAddr struct {
	label string
	addr  netip.Addr
}

func (m *Manager) zoneFor(ctx context.Context, it store.Intent) (dns.Zone, bool) {
	if it.Network == nil {
		return dns.Zone{}, false
	}
	gw, err := netip.ParseAddr(it.Network.Gateway)
	if err != nil {
		return dns.Zone{}, false
	}
	subnet, _ := netip.ParsePrefix(it.Network.Subnet)
	z := dns.Zone{Domain: Domain(it.Name), Listen: gw, Subnet: subnet, Records: map[string]netip.Addr{}}

	// Roles are added first so an instance name never shadows a role.
	var aliases []nameAddr
	var live map[string]string
	liveRead := false
	for _, mem := range it.Members {
		ip := mem.IP
		spec, specErr := m.Instances.GetByID(mem.InstanceID)
		if specErr == nil && mem.Kind == instance.KindContainer && spec.Container != nil && m.Networker != nil {
			// One network read covers every container member.
			if !liveRead {
				liveRead = true
				lookupCtx, cancel := context.WithTimeout(ctx, containerLookupTimeout)
				live, _ = m.Networker.ContainerAddresses(lookupCtx, it.Network.EngineNetworkName)
				cancel()
			}
			if addr := live[spec.Container.ContainerID]; addr != "" {
				ip = addr
			}
		}
		addr, err := netip.ParseAddr(ipam.StripCIDR(ip))
		if err != nil {
			continue
		}
		if label := dns.Label(mem.Role); label != "" {
			if _, taken := z.Records[label]; !taken {
				z.Records[label] = addr
			}
		}
		if specErr == nil {
			aliases = append(aliases, nameAddr{dns.Label(spec.Name), addr})
		}
	}
	for _, a := range aliases {
		if _, taken := z.Records[a.label]; a.label != "" && !taken {
			z.Records[a.label] = a.addr
		}
	}
	return z, true
}
