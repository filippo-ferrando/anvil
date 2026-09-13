// Package migrate implements `anvil migrate`, both a single instance
// (M5) and a whole intent as one group (M7). Per the plan's Migration
// section: there's no daemon-to-daemon gRPC trust between hosts. The
// source daemon SSHes into the target host (shelling out to the real
// `ssh`/`scp` binaries — same reasoning as the CLI's own shell/exec/
// transfer using real `ssh` instead of a Go SSH library: no new
// dependency, real known_hosts/agent handling for free) and drives the
// target's own local `anvil migrate-import` (see internal/cli/commands),
// which talks to the target's own local anvild over its own unix socket.
// Whatever SSH access already exists to a host is the only trust this
// needs.
package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/migrate/payload"
	"github.com/anvil-project/anvil/internal/store"
)

// Store is the subset of *store.Store's methods Manager needs.
type Store interface {
	GetHost(alias string) (store.Host, error)

	// GetIntentByName looks up name as a whole intent, when it doesn't
	// resolve to a single instance — see Manager.Migrate.
	GetIntentByName(name string) (store.Intent, error)
}

// Instances is the subset of *instance.Manager's methods Manager needs.
type Instances interface {
	Info(names []string) ([]*instance.Spec, error)
	Stop(ctx context.Context, names []string, force bool, timeout time.Duration) error
	Delete(ctx context.Context, names []string, purge bool) error

	// GetByID resolves an intent member's store.IntentMember.InstanceID
	// to its full spec — see Manager.migrateIntent.
	GetByID(id string) (*instance.Spec, error)
}

// Exporter flattens a VM instance's disk (backing-file overlay and all)
// into a standalone qcow2 file — implemented by internal/vm.Backend. Not
// meaningful for a container: see Manager.Migrate's container path, which
// transfers no disk data, just the spec needed to re-pull the image.
type Exporter interface {
	ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error
}

type Manager struct {
	Store     Store
	Instances Instances
	Exporter  Exporter
}

func NewManager(s Store, instances Instances, exporter Exporter) *Manager {
	return &Manager{Store: s, Instances: instances, Exporter: exporter}
}

// Params is Manager.Migrate's input.
type Params struct {
	Name string
	To   string // a saved host's alias, or a literal "user@host[:port]"

	// Copy: true leaves the source alone after a successful migration.
	// false (default): delete it, but only after the target confirms
	// success — never delete-first. For an intent, this applies
	// per-member: only members that actually succeeded get deleted from
	// the source (Result.Members says which).
	Copy bool

	DestName string // single-instance migration only; rejected if Name resolves to an intent
	DryRun   bool   // check connectivity/preconditions and report the plan; transfer nothing

	// BestEffort only applies when Name resolves to an intent (rejected
	// for a single instance): migrate every member independently
	// instead of aborting the whole group and rolling back the target
	// on the first member that fails. See Manager.migrateIntent.
	BestEffort bool
}

// Result is Manager.Migrate's output. A single-instance migration only
// ever sets InstanceID; an intent migration only ever sets IntentName,
// Members, and RolledBack.
type Result struct {
	InstanceID string

	IntentName string
	Members    []MemberResult
	RolledBack bool
}

// MemberResult is one intent member's own migration outcome.
type MemberResult struct {
	Role  string
	NewID string // set when this member migrated successfully
	Err   error  // set when it didn't
}

func (m *Manager) resolveTarget(to string) (target, error) {
	if h, err := m.Store.GetHost(to); err == nil {
		t, perr := parseTarget(h.Target)
		if perr != nil {
			return target{}, perr
		}
		t.Identity = h.Identity
		return t, nil
	}
	return parseTarget(to)
}

// CheckHost verifies aliasOrTarget is reachable over SSH and that
// `anvil`/`anvild` are actually installed there — used by
// HostService.Test and Migrate's own --dry-run path. Returns a short
// human-readable detail on success.
func (m *Manager) CheckHost(ctx context.Context, aliasOrTarget string) (string, error) {
	if _, err := m.EnsurePublicKey(); err != nil {
		return "", err
	}
	t, err := m.resolveTarget(aliasOrTarget)
	if err != nil {
		return "", err
	}
	out, err := sshRun(ctx, t, "command -v anvil && command -v anvild", nil)
	if err != nil {
		return "", err
	}
	paths := strings.Fields(out)
	if len(paths) != 2 {
		return "", fmt.Errorf("anvil/anvild not both found on PATH on the target (got: %q)", strings.TrimSpace(out))
	}
	return fmt.Sprintf("reachable, anvil at %s, anvild at %s", paths[0], paths[1]), nil
}

// GuestKey asks aliasOrTarget's own local anvil for its default
// guest-access SSH public key by running `anvil migrate-guest-key`
// there (a hidden CLI command, same fixed-command pattern as
// migrate-import) over the same SSH channel Migrate itself drives — not
// a new trust relationship, just reusing the host-to-host access
// `anvil host add`/EnsurePublicKey already established. See
// MigrateService.GuestKey's doc comment in the proto for why the CLI's
// own `anvil migrate` needs this before migrating a VM.
func (m *Manager) GuestKey(ctx context.Context, aliasOrTarget string) (string, error) {
	if _, err := m.EnsurePublicKey(); err != nil {
		return "", err
	}
	t, err := m.resolveTarget(aliasOrTarget)
	if err != nil {
		return "", err
	}
	out, err := sshRun(ctx, t, "anvil migrate-guest-key", nil)
	if err != nil {
		return "", fmt.Errorf("migrate: fetching %s's guest-access key: %w", aliasOrTarget, err)
	}
	key := strings.TrimSpace(out)
	if key == "" {
		return "", fmt.Errorf("migrate: %s returned an empty guest-access key", aliasOrTarget)
	}
	return key, nil
}

// EnsurePublicKey returns anvild's own migration SSH public key
// (config.MigrateIdentityPath()), generating a fresh passwordless
// ed25519 keypair there on first use if it doesn't exist yet.
// packaging/anvild.install already does this once at package-install
// time, but a manually-run anvild (no packaging involved, e.g. local
// dev/testing, see README's "Running it") wouldn't have one otherwise —
// this makes `anvil migrate` (and `anvil migrate-key`) work the same way
// regardless of how anvild got started. Same reasoning as
// internal/cli/commands/ssh.go's own ensureDefaultAnvilKey: a shared,
// passwordless convenience key with nothing to protect beyond "not a
// random unrelated process" doesn't need one.
func (m *Manager) EnsurePublicKey() (string, error) {
	path := config.MigrateIdentityPath()
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("migrate: checking for the migration SSH key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("migrate: creating the SSH key directory: %w", err)
		}
		keygenBin, err := exec.LookPath("ssh-keygen")
		if err != nil {
			return "", fmt.Errorf("migrate: ssh-keygen not found on PATH (needed to generate the migration SSH key)")
		}
		cmd := exec.Command(keygenBin, "-t", "ed25519", "-N", "", "-C", "anvil-migrate", "-f", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("migrate: generating the migration SSH key: %w: %s", err, out)
		}
	}

	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", fmt.Errorf("migrate: reading the migration public key: %w", err)
	}
	return strings.TrimSpace(string(pub)), nil
}

// Migrate moves p.Name (a single instance or a whole intent, see
// Result's doc comment) to p.To. See the package doc comment for the
// overall SSH-driven mechanism. A dry run (p.DryRun) returns once
// connectivity is confirmed, without transferring anything.
func (m *Manager) Migrate(ctx context.Context, p Params, progress func(status string)) (Result, error) {
	if p.Name == "" {
		return Result{}, fmt.Errorf("migrate: an instance or intent name is required")
	}
	if p.To == "" {
		return Result{}, fmt.Errorf("migrate: --to is required")
	}

	// Guarantees a migration key exists before any ssh/scp call needs
	// one (see commonArgs' own fallback to config.MigrateIdentityPath())
	// — normally already generated by packaging/anvild.install, but not
	// for a manually-run anvild.
	if _, err := m.EnsurePublicKey(); err != nil {
		return Result{}, err
	}

	t, err := m.resolveTarget(p.To)
	if err != nil {
		return Result{}, err
	}

	// p.Name is looked up as a single instance first, a whole intent
	// second — see MigrateRequest.name's doc comment in the proto for
	// why that order. Instances.Info errors out (rather than returning
	// an empty slice) for a name that matches nothing, so its error is
	// deliberately ignored here: that's exactly the signal to try the
	// intent lookup next, not a fatal problem.
	specs, _ := m.Instances.Info([]string{p.Name})
	if len(specs) > 0 {
		if p.BestEffort {
			return Result{}, fmt.Errorf("migrate: --best-effort only applies to a whole intent, %q is a single instance", p.Name)
		}
		newID, err := m.migrateInstance(ctx, t, specs[0], p, progress)
		return Result{InstanceID: newID}, err
	}

	it, err := m.Store.GetIntentByName(p.Name)
	if err != nil {
		return Result{}, fmt.Errorf("migrate: no instance or intent named %q", p.Name)
	}
	if p.DestName != "" {
		return Result{}, fmt.Errorf("migrate: --dest-name doesn't apply to a whole intent, only a single instance")
	}
	return m.migrateIntent(ctx, t, it, p, progress)
}

// migrateInstance moves one already-resolved instance to t as a
// standalone instance on the target — the M5 behavior, factored out of
// Migrate so migrateIntent (M7) can drive the same per-instance transfer
// (migrateSpec) for each of an intent's members.
func (m *Manager) migrateInstance(ctx context.Context, t target, spec *instance.Spec, p Params, progress func(status string)) (string, error) {
	destName := p.DestName
	if destName == "" {
		destName = spec.Name
	}

	if p.DryRun {
		progress(fmt.Sprintf("would migrate %q (%s) to %s@%s as %q", spec.Name, spec.Kind, t.User, t.Host, destName))
		detail, err := m.CheckHost(ctx, p.To)
		if err != nil {
			return "", fmt.Errorf("migrate: dry run connectivity check failed: %w", err)
		}
		progress("target check: " + detail)
		return "", nil
	}

	// Stopped first, unconditionally: a VM's disk can't be safely
	// flattened while a live QEMU process might still be writing to it,
	// and moving a running container out from under itself doesn't mean
	// much either. Matches the plan's "instances must be stopped first"
	// precondition.
	progress("stopping source instance")
	if err := m.Instances.Stop(ctx, []string{spec.Name}, false, 30*time.Second); err != nil {
		return "", fmt.Errorf("migrate: stopping %q: %w", spec.Name, err)
	}

	newID, err := m.migrateSpec(ctx, t, spec, destName, nil, progress)
	if err != nil {
		return "", err
	}

	if !p.Copy {
		// Only now, after the target has confirmed success — copy,
		// verify, then delete, never delete-first (see the plan's
		// Migration section).
		progress("deleting source instance")
		if err := m.Instances.Delete(ctx, []string{spec.Name}, true); err != nil {
			return "", fmt.Errorf("migrate: succeeded on target (new id %s) but failed to delete source: %w", newID, err)
		}
	}

	progress(fmt.Sprintf("done: new instance %s on %s@%s", newID, t.User, t.Host))
	return newID, nil
}

// migrateIntent moves every member of it to t as one group. Each member
// goes through the same migrateSpec transfer a standalone migration
// uses, just carrying it.Name and its own role along too — the target's
// own intent.Manager.Launch (already built for M4) is what actually
// recreates the shared network there, on the first member that arrives,
// so this package needs no copy of that logic.
//
// Critically, every member's payload also carries the source intent's
// exact subnet/gateway (see payload.IntentNetwork), and each VM member's
// own exact static address — pinning the target's newly-created intent
// to the identical network instead of letting it auto-allocate a fresh
// one, via LaunchParams.PinnedNetwork/PinnedStaticIP. This isn't an
// optimization, it's load-bearing: a migrated VM's disk skips cloud-init
// entirely on relaunch, so its already-baked-in static network config
// (and whatever peer /etc/hosts entries it already has, from its own
// original launch) has no way to catch up to a brand new subnet on its
// own. Preserving the exact same addresses for the whole group instead
// means nothing in any guest needs to change at all — the already-baked-
// in config just keeps being correct. (Container members don't need
// this: their networking is re-established fresh at every launch anyway,
// nothing baked-in to preserve, so they're simply left to the target's
// own engine to assign an address as usual.)
//
// Default mode (p.BestEffort false) is all-or-nothing: the first member
// failure stops the migration there, whatever already landed on the
// target gets deleted via the target's own `anvil migrate-rollback`
// (best-effort cleanup itself — a leftover instance there is logged, not
// fatal), and every source member is left exactly as stopping it left
// it, never deleted, since a source member only ever gets deleted after
// its own migration is confirmed successful.
// --best-effort instead keeps going through every member regardless of
// earlier failures, deletes only the ones that actually succeeded from
// the source, and leaves the rest (already stopped, never migrated) in
// place — Result.Members says exactly which is which either way. One
// real limitation this doesn't solve: a member left behind on the source
// still has the migrated peers' *old* addresses baked into its own
// /etc/hosts, now pointing at a bridge those peers are no longer
// attached to — an accepted, documented gap in partial migration, not
// something this package tries to fix live.
func (m *Manager) migrateIntent(ctx context.Context, t target, it store.Intent, p Params, progress func(status string)) (Result, error) {
	if len(it.Members) == 0 {
		return Result{}, fmt.Errorf("migrate: intent %q has no members", it.Name)
	}

	var netPayload *payload.IntentNetwork
	var subnetPrefix string // e.g. "/24", derived from it.Network.Subnet, to reattach to a bare member IP
	if it.Network != nil {
		netPayload = &payload.IntentNetwork{
			Subnet:        it.Network.Subnet,
			Gateway:       it.Network.Gateway,
			DockerIPRange: it.Network.DockerIPRange,
		}
		if _, after, ok := strings.Cut(it.Network.Subnet, "/"); ok {
			subnetPrefix = "/" + after
		}
	}

	type resolvedMember struct {
		spec     *instance.Spec
		role     string
		staticIP string // this member's own pinned address (VM only, CIDR form); "" if none
	}
	members := make([]resolvedMember, 0, len(it.Members))
	names := make([]string, 0, len(it.Members))
	for _, mem := range it.Members {
		spec, err := m.Instances.GetByID(mem.InstanceID)
		if err != nil {
			return Result{}, fmt.Errorf("migrate: intent %q member %q: %w", it.Name, mem.Role, err)
		}
		staticIP := ""
		if spec.Kind == instance.KindVM && mem.IP != "" && subnetPrefix != "" {
			staticIP = mem.IP + subnetPrefix
		}
		members = append(members, resolvedMember{spec: spec, role: mem.Role, staticIP: staticIP})
		names = append(names, spec.Name)
	}

	if p.DryRun {
		for _, mem := range members {
			progress(fmt.Sprintf("would migrate %q (%s, role %q) to %s@%s", mem.spec.Name, mem.spec.Kind, mem.role, t.User, t.Host))
		}
		detail, err := m.CheckHost(ctx, p.To)
		if err != nil {
			return Result{}, fmt.Errorf("migrate: dry run connectivity check failed: %w", err)
		}
		progress("target check: " + detail)
		return Result{IntentName: it.Name}, nil
	}

	progress(fmt.Sprintf("stopping %d source instance(s)", len(names)))
	if err := m.Instances.Stop(ctx, names, false, 30*time.Second); err != nil {
		return Result{}, fmt.Errorf("migrate: stopping intent %q's members: %w", it.Name, err)
	}

	var results []MemberResult
	var succeededNames []string
	failed := false
	for _, mem := range members {
		progress(fmt.Sprintf("migrating %q (role %q)", mem.spec.Name, mem.role))
		im := &intentMigration{name: it.Name, role: mem.role, network: netPayload, staticIP: mem.staticIP}
		newID, err := m.migrateSpec(ctx, t, mem.spec, mem.spec.Name, im, progress)
		if err != nil {
			failed = true
			results = append(results, MemberResult{Role: mem.role, Err: err})
			progress(fmt.Sprintf("member %q failed: %v", mem.role, err))
			if !p.BestEffort {
				// No point migrating the rest of the group just to roll
				// it all back a moment later.
				break
			}
			continue
		}
		results = append(results, MemberResult{Role: mem.role, NewID: newID})
		succeededNames = append(succeededNames, mem.spec.Name)
	}

	rolledBack := false
	switch {
	case failed && !p.BestEffort:
		if len(succeededNames) > 0 {
			progress(fmt.Sprintf("rolling back %d already-migrated member(s) on target", len(succeededNames)))
			if err := rollbackRemote(ctx, t, succeededNames); err != nil {
				progress(fmt.Sprintf("rollback warning: %v", err))
			}
		}
		rolledBack = true
	case len(succeededNames) > 0 && !p.Copy:
		progress(fmt.Sprintf("deleting %d migrated source instance(s)", len(succeededNames)))
		if err := m.Instances.Delete(ctx, succeededNames, true); err != nil {
			progress(fmt.Sprintf("warning: migrated successfully but failed to delete source: %v", err))
		}
	}

	progress(fmt.Sprintf("done: %d of %d member(s) migrated", len(succeededNames), len(members)))
	return Result{IntentName: it.Name, Members: results, RolledBack: rolledBack}, nil
}

// intentMigration bundles everything migrateSpec needs when its instance
// is one member of a whole-intent migration — nil for a standalone
// migration. Kept as one struct rather than an ever-growing parameter
// list, since it's really one cohesive "this instance belongs to an
// intent" concept: the intent's name, this member's role, and (see
// migrateIntent's doc comment) the pinned network/address that let the
// member's already-baked-in guest config keep working unchanged.
type intentMigration struct {
	name     string
	role     string
	network  *payload.IntentNetwork // nil if the intent has no shared network
	staticIP string                 // this member's own pinned address (VM only, CIDR form); "" if none
}

// migrateSpec transfers spec's data (a VM's flattened disk via scp, a
// container's just its spec, no data transfer) and relaunches it on t as
// destName, via the target's own `anvil migrate-import`. im is nil for a
// standalone migration; set for one member of a whole intent
// (migrateIntent).
func (m *Manager) migrateSpec(ctx context.Context, t target, spec *instance.Spec, destName string, im *intentMigration, progress func(status string)) (string, error) {
	pl := payload.Payload{Name: destName, Kind: string(spec.Kind)}
	if im != nil {
		pl.IntentName = im.name
		pl.Role = im.role
		pl.IntentNetwork = im.network
		pl.StaticIP = im.staticIP
	}

	switch spec.Kind {
	case instance.KindVM:
		if m.Exporter == nil {
			return "", fmt.Errorf("migrate: no VM exporter configured")
		}
		if err := m.buildVMPayload(ctx, spec, t, &pl, progress); err != nil {
			return "", err
		}
	case instance.KindContainer:
		buildContainerPayload(spec, &pl)
	default:
		return "", fmt.Errorf("migrate: unsupported instance kind %q", spec.Kind)
	}

	data, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("migrate: encoding payload: %w", err)
	}

	progress("launching on target")
	out, runErr := sshRun(ctx, t, "anvil migrate-import", bytes.NewReader(data))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "MIGRATE_OK ") || strings.HasPrefix(line, "MIGRATE_FAIL ") {
			continue
		}
		progress("target: " + line)
	}
	if runErr != nil {
		return "", fmt.Errorf("migrate: launching on target: %w", runErr)
	}

	newID, importErr := parseMigrateResult(out)
	if importErr != nil {
		return "", fmt.Errorf("migrate: target reported failure: %w", importErr)
	}
	return newID, nil
}

// rollbackRemote asks t's own local anvil to force-delete already-
// migrated instance names — used by migrateIntent when an all-or-nothing
// migration fails partway through. Same fixed, argument-free remote
// command plus stdin-JSON pattern as `anvil migrate-import`, for the
// same reason: names travel over stdin, never as command-line arguments.
func rollbackRemote(ctx context.Context, t target, names []string) error {
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("migrate: encoding rollback names: %w", err)
	}
	_, err = sshRun(ctx, t, "anvil migrate-rollback", bytes.NewReader(data))
	return err
}

// buildVMPayload flattens spec's disk, ships it to t, and fills in pl.VM
// with everything anvil migrate-import needs to relaunch it — including
// where the disk landed on the target.
func (m *Manager) buildVMPayload(ctx context.Context, spec *instance.Spec, t target, pl *payload.Payload, progress func(status string)) error {
	stagingDir := config.MigrateStagingDir()
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return fmt.Errorf("migrate: creating staging dir: %w", err)
	}
	localPath := filepath.Join(stagingDir, spec.ID+".qcow2")
	defer os.Remove(localPath)

	progress("flattening disk")
	if err := m.Exporter.ExportDisk(ctx, spec, localPath); err != nil {
		return fmt.Errorf("migrate: exporting disk: %w", err)
	}

	remotePath := "/tmp/anvil-migrate-" + spec.ID + ".qcow2"
	if err := scpUpload(ctx, t, localPath, remotePath, progress); err != nil {
		return err
	}

	pl.VM = &payload.VM{
		ImageRef:       spec.VM.ImageRef,
		Arch:           spec.VM.Arch,
		CPUs:           int32(spec.VM.CPUs),
		MemoryMiB:      spec.VM.MemoryMiB,
		DefaultUser:    spec.VM.DefaultUser,
		RemoteDiskPath: remotePath,
	}
	return nil
}

// buildContainerPayload fills in pl.Container from spec — no data
// transfer, the target re-pulls ImageRef itself (only actually works for
// a registry-hosted image, see payload.Container's doc comment).
func buildContainerPayload(spec *instance.Spec, pl *payload.Payload) {
	c := spec.Container
	pc := &payload.Container{
		ImageRef:   c.ImageRef,
		Env:        c.Env,
		Entrypoint: c.Entrypoint,
		Cmd:        c.Cmd,
		// Left blank even for a container that's an intent member on the
		// source: when migrateSpec sets pl.IntentName, the target's own
		// intent.Manager.Launch overwrites this with its own network's
		// name regardless (see its doc comment); for a standalone
		// migration there's simply no shared network to carry over.
		NetworkMode: "",
		Engine:      string(c.Engine),
	}
	for _, v := range c.Volumes {
		pc.Volumes = append(pc.Volumes, payload.VolumeMount{
			HostPath: v.HostPath, ContainerPath: v.ContainerPath, ReadOnly: v.ReadOnly,
		})
	}
	for _, port := range c.Ports {
		pc.Ports = append(pc.Ports, payload.PortMapping{
			HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: port.Protocol,
		})
	}
	pl.Container = pc
}

// parseMigrateResult scans anvil migrate-import's captured stdout for its
// final MIGRATE_OK/MIGRATE_FAIL marker line — everything else in that
// output is forwarded as progress (see Migrate), this is just the
// unambiguous machine-readable result the human-readable lines around it
// aren't safe to parse for.
func parseMigrateResult(out string) (id string, err error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "MIGRATE_OK "); ok {
			return v, nil
		}
		if v, ok := strings.CutPrefix(line, "MIGRATE_FAIL "); ok {
			return "", fmt.Errorf("%s", v)
		}
	}
	return "", fmt.Errorf("target produced no result (got: %q)", out)
}
