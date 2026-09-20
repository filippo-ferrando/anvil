package export

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// Store is the subset of *store.Store's methods Manager needs.
type Store interface {
	GetIntentByName(name string) (store.Intent, error)
	GetCloudInit(name string) (store.CloudInitConfig, error)
}

// Instances is the subset of *instance.Manager's methods Manager needs.
type Instances interface {
	Info(names []string) ([]*instance.Spec, error)
	GetByID(id string) (*instance.Spec, error)
	Start(ctx context.Context, names []string) error
	Stop(ctx context.Context, names []string, force bool, timeout time.Duration) error
	Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error
}

// Intents is the subset of *intent.Manager's methods Manager needs.
type Intents interface {
	Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error
}

// VMImporter prepares an imported VM's diff disk for adoption on this host.
type VMImporter interface {
	PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath string) error
}

type Manager struct {
	Store      Store
	Instances  Instances
	Intents    Intents
	VMImporter VMImporter
}

func NewManager(s Store, instances Instances, intents Intents, vmImporter VMImporter) *Manager {
	return &Manager{Store: s, Instances: instances, Intents: intents, VMImporter: vmImporter}
}

// ExportParams is Manager.Export's input.
type ExportParams struct {
	Name       string // resolved as a single instance first, a whole intent second, unless IsIntent
	OutputPath string

	// IsIntent skips instance resolution and looks Name up as an intent only. Use it when the caller
	// already knows Name is an intent, so an unrelated instance with the same name can't shadow it.
	IsIntent bool
}

// Export packages p.Name into a tar.zst bundle at p.OutputPath. Every member is stopped for a
// consistent snapshot, then resumed to its prior state, even on failure.
func (m *Manager) Export(ctx context.Context, p ExportParams, progress func(status string)) error {
	if p.Name == "" {
		return fmt.Errorf("export: a name is required")
	}
	if p.OutputPath == "" {
		return fmt.Errorf("export: an output path is required")
	}

	manifest, members, err := m.resolveExportTargets(p.Name, p.IsIntent)
	if err != nil {
		return err
	}

	names := make([]string, len(members))
	wasRunning := make(map[string]bool, len(members))
	for i, spec := range members {
		names[i] = spec.Name
		wasRunning[spec.Name] = spec.State == instance.StateRunning
	}

	progress(fmt.Sprintf("stopping %d instance(s)", len(names)))
	if err := m.Instances.Stop(ctx, names, false, 30*time.Second); err != nil {
		return fmt.Errorf("export: stopping source instance(s): %w", err)
	}
	defer func() {
		var restart []string
		for _, name := range names {
			if wasRunning[name] {
				restart = append(restart, name)
			}
		}
		if len(restart) == 0 {
			return
		}
		progress(fmt.Sprintf("resuming %d instance(s)", len(restart)))
		if err := m.Instances.Start(ctx, restart); err != nil {
			progress(fmt.Sprintf("warning: some instances failed to resume after export: %v", err))
		}
	}()

	type diskSource struct{ archivePath, localPath string }
	type dirSource struct{ archivePath, localPath string }
	var disks []diskSource
	var dirs []dirSource

	for _, spec := range members {
		progress(fmt.Sprintf("archiving %q", spec.Name))
		mem := Member{Name: spec.Name, Kind: string(spec.Kind)}
		if manifest.IntentName != "" {
			mem.Role = spec.Labels[instance.RoleLabel]
			if mem.Role == "" {
				mem.Role = spec.Name
			}
		}

		switch spec.Kind {
		case instance.KindVM:
			v := spec.VM
			if manifest.Network != nil && v.StaticIP != "" {
				mem.StaticIP = v.StaticIP
			}
			mem.VM = &VM{
				ImageRef:         v.ImageRef,
				Arch:             v.Arch,
				CPUs:             int32(v.CPUs),
				MemoryMiB:        v.MemoryMiB,
				DiskGiB:          v.DiskGiB,
				CloudInitContent: m.resolveCloudInit(v),
				SSHPublicKeys:    v.SSHPublicKeys,
				DiskFile:         "disks/" + spec.ID + ".qcow2",
			}
			for _, port := range v.Ports {
				mem.VM.Ports = append(mem.VM.Ports, PortMapping{HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: port.Protocol})
			}
			disks = append(disks, diskSource{archivePath: mem.VM.DiskFile, localPath: v.DiskPath})

		case instance.KindContainer:
			c := spec.Container
			mem.Container = &Container{
				ImageRef: c.ImageRef, Env: c.Env, Entrypoint: c.Entrypoint, Cmd: c.Cmd, Engine: string(c.Engine),
			}
			for _, port := range c.Ports {
				mem.Container.Ports = append(mem.Container.Ports, PortMapping{HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: port.Protocol})
			}
			for i, v := range c.Volumes {
				vol := VolumeMount{ContainerPath: v.ContainerPath, ReadOnly: v.ReadOnly}
				if info, err := os.Stat(v.HostPath); err == nil && info.IsDir() {
					vol.Archive = fmt.Sprintf("volumes/%s/%d", spec.ID, i)
					dirs = append(dirs, dirSource{archivePath: vol.Archive, localPath: v.HostPath})
				} else {
					progress(fmt.Sprintf("warning: %q's volume %s isn't a directory anvild can read, skipping its contents", spec.Name, v.HostPath))
				}
				mem.Container.Volumes = append(mem.Container.Volumes, vol)
			}

		default:
			return fmt.Errorf("export: unsupported instance kind %q", spec.Kind)
		}

		manifest.Members = append(manifest.Members, mem)
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("export: encoding manifest: %w", err)
	}

	progress("writing bundle")
	err = writeTarZst(ctx, p.OutputPath, func(tw *tar.Writer) error {
		if err := addBytesToTar(tw, "manifest.json", manifestJSON); err != nil {
			return err
		}
		for _, d := range disks {
			if err := addFileToTar(tw, d.archivePath, d.localPath); err != nil {
				return fmt.Errorf("archiving disk %s: %w", d.localPath, err)
			}
		}
		for _, d := range dirs {
			if err := addDirToTar(tw, d.archivePath, d.localPath); err != nil {
				return fmt.Errorf("archiving volume %s: %w", d.localPath, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("export: writing bundle: %w", err)
	}

	progress("done: " + p.OutputPath)
	return nil
}

// resolveCloudInit returns v's effective cloud-init user-data as literal content, resolving a named
// library entry if v uses one, so the bundle carries the content itself rather than a name the target might lack.
func (m *Manager) resolveCloudInit(v *instance.VMSpec) string {
	if v.CloudInitName == "" {
		return v.CloudInitUserData
	}
	rec, err := m.Store.GetCloudInit(v.CloudInitName)
	if err != nil {
		return ""
	}
	return rec.Content
}

// resolveExportTargets looks up name as a single instance first, a whole intent second (same as
// migrate.Manager.Migrate), unless isIntent skips straight to the intent lookup so a same-named instance can't shadow it.
func (m *Manager) resolveExportTargets(name string, isIntent bool) (Manifest, []*instance.Spec, error) {
	if isIntent {
		return m.resolveIntentTarget(name)
	}

	specs, err := m.Instances.Info([]string{name})
	if err != nil && !errors.Is(err, instance.ErrNotFound) {
		return Manifest{}, nil, fmt.Errorf("export: looking up %q: %w", name, err)
	}
	if len(specs) > 0 {
		if intentName := specs[0].Labels[instance.IntentLabel]; intentName != "" {
			return Manifest{}, nil, fmt.Errorf(
				"export: %q is a member of intent %q; export the whole intent (`anvil export %s`), not the member by its own instance name",
				name, intentName, intentName)
		}
		return Manifest{}, specs, nil
	}

	return m.resolveIntentTarget(name)
}

// resolveIntentTarget looks name up as an intent only.
func (m *Manager) resolveIntentTarget(name string) (Manifest, []*instance.Spec, error) {
	it, err := m.Store.GetIntentByName(name)
	if err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return Manifest{}, nil, fmt.Errorf("export: no instance or intent named %q", name)
		}
		return Manifest{}, nil, fmt.Errorf("export: looking up %q as an intent: %w", name, err)
	}
	if len(it.Members) == 0 {
		return Manifest{}, nil, fmt.Errorf("export: intent %q has no members", it.Name)
	}

	manifest := Manifest{IntentName: it.Name}
	if it.Network != nil {
		manifest.Network = &Network{Subnet: it.Network.Subnet, Gateway: it.Network.Gateway, DockerIPRange: it.Network.DockerIPRange}
	}
	members := make([]*instance.Spec, 0, len(it.Members))
	for _, mem := range it.Members {
		spec, err := m.Instances.GetByID(mem.InstanceID)
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("export: intent %q member %q: %w", it.Name, mem.Role, err)
		}
		members = append(members, spec)
	}
	return manifest, members, nil
}

// ImportParams is Manager.Import's input.
type ImportParams struct {
	BundlePath string
	Name       string // renames the imported intent, or the instance for a standalone bundle
}

// Result is Manager.Import's output.
type Result struct {
	IntentName string // empty for a standalone instance import
	Members    []MemberResult
}

// MemberResult is one member's own import outcome.
type MemberResult struct {
	Role  string
	NewID string
}

// Import extracts p.BundlePath and relaunches every member it contains.
func (m *Manager) Import(ctx context.Context, p ImportParams, progress func(status string)) (Result, error) {
	if p.BundlePath == "" {
		return Result{}, fmt.Errorf("import: a bundle path is required")
	}

	stagingDir, err := os.MkdirTemp(config.ExportStagingDir(), "import-*")
	if err != nil {
		return Result{}, fmt.Errorf("import: creating staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	progress("extracting bundle")
	if err := extractTarZst(ctx, p.BundlePath, stagingDir); err != nil {
		return Result{}, fmt.Errorf("import: extracting bundle: %w", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(stagingDir, "manifest.json"))
	if err != nil {
		return Result{}, fmt.Errorf("import: bundle has no manifest.json: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Result{}, fmt.Errorf("import: invalid manifest.json: %w", err)
	}

	intentName := manifest.IntentName
	if manifest.IntentName != "" && p.Name != "" {
		intentName = p.Name
	}
	var pinnedNetwork *instance.PinnedNetwork
	if manifest.Network != nil {
		pinnedNetwork = &instance.PinnedNetwork{
			Subnet: manifest.Network.Subnet, Gateway: manifest.Network.Gateway, DockerIPRange: manifest.Network.DockerIPRange,
		}
	}

	result := Result{IntentName: intentName}
	for _, mem := range manifest.Members {
		destName := mem.Name
		if manifest.IntentName == "" && p.Name != "" {
			destName = p.Name
		}
		progress(fmt.Sprintf("importing %q", destName))

		params := instance.LaunchParams{Name: destName}
		if intentName != "" {
			params.IntentName = intentName
			params.Role = mem.Role
			params.PinnedNetwork = pinnedNetwork
			params.PinnedStaticIP = mem.StaticIP
		}

		if err := m.fillLaunchParams(ctx, &params, mem, destName, stagingDir, progress); err != nil {
			return Result{}, fmt.Errorf("import: preparing %q: %w", destName, err)
		}

		launch := m.Instances.Launch
		if params.IntentName != "" {
			launch = m.Intents.Launch
		}
		var launched *instance.Spec
		var launchErr error
		if err := launch(ctx, params, func(ev instance.LaunchEvent) {
			if ev.Status != "" {
				progress(ev.Status)
			}
			if ev.Instance != nil {
				launched = ev.Instance
			}
			if ev.Err != nil {
				launchErr = ev.Err
			}
		}); err != nil {
			return Result{}, fmt.Errorf("import: launching %q: %w", destName, err)
		}
		if launchErr != nil {
			return Result{}, fmt.Errorf("import: launching %q: %w", destName, launchErr)
		}
		result.Members = append(result.Members, MemberResult{Role: mem.Role, NewID: launched.ID})
	}

	progress("done")
	return result, nil
}

// fillLaunchParams resolves mem's disk/volumes out of stagingDir and fills
// params.Kind/VM/Container accordingly.
func (m *Manager) fillLaunchParams(ctx context.Context, params *instance.LaunchParams, mem Member, destName, stagingDir string, progress func(string)) error {
	switch mem.Kind {
	case "vm":
		if mem.VM == nil {
			return fmt.Errorf("member has no VM spec")
		}
		diskPath := filepath.Join(stagingDir, mem.VM.DiskFile)
		if err := m.VMImporter.PrepareImportedDisk(ctx, mem.VM.ImageRef, mem.VM.Arch, diskPath); err != nil {
			return fmt.Errorf("preparing disk: %w", err)
		}
		v := &instance.VMSpec{
			ImageRef:          mem.VM.ImageRef,
			Arch:              mem.VM.Arch,
			CPUs:              int(mem.VM.CPUs),
			MemoryMiB:         mem.VM.MemoryMiB,
			DiskGiB:           mem.VM.DiskGiB,
			CloudInitUserData: mem.VM.CloudInitContent,
			SSHPublicKeys:     mem.VM.SSHPublicKeys,
			SourceDiskPath:    diskPath,
		}
		for _, p := range mem.VM.Ports {
			v.Ports = append(v.Ports, instance.PortMapping{HostPort: p.HostPort, GuestPort: p.GuestPort, Protocol: p.Protocol})
		}
		params.Kind = instance.KindVM
		params.VM = v

	case "container":
		if mem.Container == nil {
			return fmt.Errorf("member has no container spec")
		}
		c := &instance.ContainerSpec{
			ImageRef: mem.Container.ImageRef, Env: mem.Container.Env,
			Entrypoint: mem.Container.Entrypoint, Cmd: mem.Container.Cmd,
			Engine: instance.ContainerEngine(mem.Container.Engine),
		}
		for _, p := range mem.Container.Ports {
			c.Ports = append(c.Ports, instance.PortMapping{HostPort: p.HostPort, GuestPort: p.GuestPort, Protocol: p.Protocol})
		}
		for i, vol := range mem.Container.Volumes {
			if vol.Archive == "" {
				progress(fmt.Sprintf("warning: %q's volume for %s had no archived contents, skipping it", destName, vol.ContainerPath))
				continue
			}
			hostPath := config.ImportedVolumeDir(destName, i)
			if err := moveDir(filepath.Join(stagingDir, vol.Archive), hostPath); err != nil {
				return fmt.Errorf("restoring volume %d: %w", i, err)
			}
			c.Volumes = append(c.Volumes, instance.VolumeMount{HostPath: hostPath, ContainerPath: vol.ContainerPath, ReadOnly: vol.ReadOnly})
		}
		params.Kind = instance.KindContainer
		params.Container = c

	default:
		return fmt.Errorf("unknown member kind %q", mem.Kind)
	}
	return nil
}

// moveDir relocates src to dst, falling back to a recursive copy when a plain rename can't cross
// filesystems (src and dst can live under different XDG dirs); same idiom as internal/vm.Backend.adoptMigratedDisk.
func moveDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = out.ReadFrom(in)
		return err
	})
}
