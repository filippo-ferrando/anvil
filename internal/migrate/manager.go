// Package migrate implements `anvil migrate`, moving a single instance or
// a whole intent to another host over SSH.
package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	// GetIntentByName looks up name as a whole intent.
	GetIntentByName(name string) (store.Intent, error)
}

// Instances is the subset of *instance.Manager's methods Manager needs.
type Instances interface {
	Info(names []string) ([]*instance.Spec, error)
	Stop(ctx context.Context, names []string, force bool, timeout time.Duration) error
	Delete(ctx context.Context, names []string, purge bool) error

	// GetByID resolves an intent member's InstanceID to its full spec.
	GetByID(id string) (*instance.Spec, error)
}

// Exporter flattens a VM instance's disk into a standalone qcow2 file.
type Exporter interface {
	ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error
}

// IntentCleanup removes a migrated member from its source intent's
// membership list once deleted. A nil IntentCleanup skips this.
type IntentCleanup interface {
	Remove(name, member string) (store.Intent, error)
}

type Manager struct {
	Store         Store
	Instances     Instances
	Exporter      Exporter
	IntentCleanup IntentCleanup
}

func NewManager(s Store, instances Instances, exporter Exporter, intentCleanup IntentCleanup) *Manager {
	return &Manager{Store: s, Instances: instances, Exporter: exporter, IntentCleanup: intentCleanup}
}

// Params is Manager.Migrate's input.
type Params struct {
	Name string
	To   string // a saved host's alias, or a literal "user@host[:port]"

	// Copy: true leaves the source alone after a successful migration;
	// false deletes it once the target confirms success.
	Copy bool

	DestName string // single-instance migration only
	DryRun   bool   // check connectivity/preconditions and report the plan; transfer nothing

	// BestEffort, for an intent migration, migrates every member
	// independently instead of aborting and rolling back on first failure.
	BestEffort bool
}

// Result is Manager.Migrate's output: InstanceID for a single instance,
// or IntentName/Members/RolledBack for an intent.
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

// CheckHost verifies aliasOrTarget is reachable over SSH with anvil/anvild
// installed, returning a short detail string on success.
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
// guest-access SSH public key by running `anvil migrate-guest-key` there.
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

// EnsurePublicKey returns anvild's migration SSH public key, generating a
// fresh ed25519 keypair if one doesn't exist yet.
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

// Migrate moves p.Name (a single instance or a whole intent) to p.To. A
// dry run returns once connectivity is confirmed, transferring nothing.
func (m *Manager) Migrate(ctx context.Context, p Params, progress func(status string)) (Result, error) {
	if p.Name == "" {
		return Result{}, fmt.Errorf("migrate: an instance or intent name is required")
	}
	if p.To == "" {
		return Result{}, fmt.Errorf("migrate: --to is required")
	}

	if _, err := m.EnsurePublicKey(); err != nil {
		return Result{}, err
	}

	t, err := m.resolveTarget(p.To)
	if err != nil {
		return Result{}, err
	}

	// p.Name is looked up as a single instance first, a whole intent second.
	specs, err := m.Instances.Info([]string{p.Name})
	if err != nil && !errors.Is(err, instance.ErrNotFound) {
		return Result{}, fmt.Errorf("migrate: looking up %q: %w", p.Name, err)
	}
	if len(specs) > 0 {
		if p.BestEffort {
			return Result{}, fmt.Errorf("migrate: --best-effort only applies to a whole intent, %q is a single instance", p.Name)
		}
		if intentName := specs[0].Labels["intent"]; intentName != "" {
			// A member of an intent must migrate as part of the whole
			// intent, not as a standalone instance.
			return Result{}, fmt.Errorf(
				"migrate: %q is a member of intent %q — migrate the whole intent (`anvil migrate %s --to ...`) "+
					"so its network config carries over correctly, not the member by its own instance name",
				p.Name, intentName, intentName)
		}
		newID, err := m.migrateInstance(ctx, t, specs[0], p, progress)
		return Result{InstanceID: newID}, err
	}

	it, err := m.Store.GetIntentByName(p.Name)
	if err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return Result{}, fmt.Errorf("migrate: no instance or intent named %q", p.Name)
		}
		return Result{}, fmt.Errorf("migrate: looking up %q as an intent: %w", p.Name, err)
	}
	if p.DestName != "" {
		return Result{}, fmt.Errorf("migrate: --dest-name doesn't apply to a whole intent, only a single instance")
	}
	return m.migrateIntent(ctx, t, it, p, progress)
}

// migrateInstance moves one already-resolved instance to t as a
// standalone instance on the target.
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

	// The instance must be stopped before its disk can be safely flattened.
	progress("stopping source instance")
	if err := m.Instances.Stop(ctx, []string{spec.Name}, false, 30*time.Second); err != nil {
		return "", fmt.Errorf("migrate: stopping %q: %w", spec.Name, err)
	}

	newID, err := m.migrateSpec(ctx, t, spec, destName, nil, progress)
	if err != nil {
		return "", err
	}

	if !p.Copy {
		// Only delete the source after the target has confirmed success.
		progress("deleting source instance")
		if err := m.Instances.Delete(ctx, []string{spec.Name}, true); err != nil {
			return "", fmt.Errorf("migrate: succeeded on target (new id %s) but failed to delete source: %w", newID, err)
		}
	}

	progress(fmt.Sprintf("done: new instance %s on %s@%s", newID, t.User, t.Host))
	return newID, nil
}

// migrateIntent moves every member of it to t as one group, pinning each
// member's network/address to the source's own.
func (m *Manager) migrateIntent(ctx context.Context, t target, it store.Intent, p Params, progress func(status string)) (Result, error) {
	if len(it.Members) == 0 {
		return Result{}, fmt.Errorf("migrate: intent %q has no members", it.Name)
	}

	var netPayload *payload.IntentNetwork
	var subnetPrefix string // e.g. "/24", to reattach to a bare member IP
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

	// succeeded tracks each successfully migrated member's source instance
	// name and its index in results.
	type succeededMember struct {
		name string
		idx  int
	}
	var results []MemberResult
	var succeeded []succeededMember
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
				break
			}
			continue
		}
		results = append(results, MemberResult{Role: mem.role, NewID: newID})
		succeeded = append(succeeded, succeededMember{name: mem.spec.Name, idx: len(results) - 1})
	}

	rolledBack := false
	switch {
	case failed && !p.BestEffort:
		if len(succeeded) > 0 {
			names := make([]string, len(succeeded))
			for i, s := range succeeded {
				names[i] = s.name
			}
			progress(fmt.Sprintf("rolling back %d already-migrated member(s) on target", len(succeeded)))
			if err := rollbackRemote(ctx, t, names); err != nil {
				progress(fmt.Sprintf("rollback warning: %v", err))
			}
		}
		rolledBack = true
	case len(succeeded) > 0 && !p.Copy:
		progress(fmt.Sprintf("deleting %d migrated source instance(s)", len(succeeded)))
		// Deleted one at a time so a failure on one member doesn't stop
		// the rest from being cleaned up.
		for _, s := range succeeded {
			if err := m.Instances.Delete(ctx, []string{s.name}, true); err != nil {
				progress(fmt.Sprintf("warning: %q migrated successfully but failed to delete source: %v", s.name, err))
				results[s.idx].Err = fmt.Errorf("migrated successfully but failed to delete source: %w", err)
				continue
			}
			if m.IntentCleanup != nil {
				if _, err := m.IntentCleanup.Remove(it.Name, s.name); err != nil {
					log.Printf("migrate: removing migrated member %q from source intent %q: %v", s.name, it.Name, err)
				}
			}
		}
	}

	progress(fmt.Sprintf("done: %d of %d member(s) migrated", len(succeeded), len(members)))
	return Result{IntentName: it.Name, Members: results, RolledBack: rolledBack}, nil
}

// intentMigration bundles what migrateSpec needs for an intent member;
// nil for a standalone migration.
type intentMigration struct {
	name     string
	role     string
	network  *payload.IntentNetwork // nil if the intent has no shared network
	staticIP string                 // this member's own pinned address (VM only, CIDR form); "" if none
}

// migrateSpec transfers spec's data and relaunches it on t as destName.
// im is nil for a standalone migration.
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
// migrated instance names.
func rollbackRemote(ctx context.Context, t target, names []string) error {
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("migrate: encoding rollback names: %w", err)
	}
	_, err = sshRun(ctx, t, "anvil migrate-rollback", bytes.NewReader(data))
	return err
}

// buildVMPayload flattens spec's disk, ships it to t, and fills in pl.VM
// with everything anvil migrate-import needs to relaunch it.
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

// buildContainerPayload fills in pl.Container from spec; the target
// re-pulls ImageRef itself, no data transfer.
func buildContainerPayload(spec *instance.Spec, pl *payload.Payload) {
	c := spec.Container
	pc := &payload.Container{
		ImageRef:   c.ImageRef,
		Env:        c.Env,
		Entrypoint: c.Entrypoint,
		Cmd:        c.Cmd,
		// The target's own launch path assigns the network mode.
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
// final MIGRATE_OK/MIGRATE_FAIL marker line.
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
