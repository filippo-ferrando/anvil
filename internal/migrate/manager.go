// Package migrate implements `anvil migrate` (M5: a single instance; a
// whole intent is M6, not built yet). Per the plan's Migration section:
// there's no daemon-to-daemon gRPC trust between hosts. The source
// daemon SSHes into the target host (shelling out to the real `ssh`/`scp`
// binaries — same reasoning as the CLI's own shell/exec/transfer using
// real `ssh` instead of a Go SSH library: no new dependency, real
// known_hosts/agent handling for free) and drives the target's own local
// `anvil migrate-import` (see internal/cli/commands), which talks to the
// target's own local anvild over its own unix socket. Whatever SSH access
// already exists to a host is the only trust this needs.
package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
}

// Instances is the subset of *instance.Manager's methods Manager needs.
type Instances interface {
	Info(names []string) ([]*instance.Spec, error)
	Stop(ctx context.Context, names []string, force bool, timeout time.Duration) error
	Delete(ctx context.Context, names []string, purge bool) error
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
	Name     string
	To       string // a saved host's alias, or a literal "user@host[:port]"
	Copy     bool   // true: leave the source alone after a successful migration. false (default): delete it, but only after the target confirms success — never delete-first
	DestName string // optional rename on the target; empty means keep the source's own name
	DryRun   bool   // check connectivity/preconditions and report the plan; transfer nothing
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

// Migrate moves p.Name to p.To, returning the new instance's ID on the
// target host on success. See the package doc comment for the overall
// SSH-driven mechanism. A dry run (p.DryRun) returns an empty ID and a
// nil error once connectivity is confirmed, without transferring
// anything.
func (m *Manager) Migrate(ctx context.Context, p Params, progress func(status string)) (string, error) {
	if p.Name == "" {
		return "", fmt.Errorf("migrate: an instance name is required")
	}
	if p.To == "" {
		return "", fmt.Errorf("migrate: --to is required")
	}

	t, err := m.resolveTarget(p.To)
	if err != nil {
		return "", err
	}

	specs, err := m.Instances.Info([]string{p.Name})
	if err != nil {
		return "", err
	}
	if len(specs) == 0 {
		return "", fmt.Errorf("migrate: no such instance %q", p.Name)
	}
	spec := specs[0]

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
	if err := m.Instances.Stop(ctx, []string{p.Name}, false, 30*time.Second); err != nil {
		return "", fmt.Errorf("migrate: stopping %q: %w", p.Name, err)
	}

	pl := payload.Payload{Name: destName, Kind: string(spec.Kind)}

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

	if !p.Copy {
		// Only now, after the target has confirmed success — copy,
		// verify, then delete, never delete-first (see the plan's
		// Migration section).
		progress("deleting source instance")
		if err := m.Instances.Delete(ctx, []string{p.Name}, true); err != nil {
			return "", fmt.Errorf("migrate: succeeded on target (new id %s) but failed to delete source: %w", newID, err)
		}
	}

	progress(fmt.Sprintf("done: new instance %s on %s@%s", newID, t.User, t.Host))
	return newID, nil
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
	progress("uploading disk to target")
	if err := scpUpload(ctx, t, localPath, remotePath); err != nil {
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
		ImageRef:    c.ImageRef,
		Env:         c.Env,
		Entrypoint:  c.Entrypoint,
		Cmd:         c.Cmd,
		NetworkMode: "", // an intent's shared network doesn't exist on the target; standalone only for M5
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
