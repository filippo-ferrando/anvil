// Package container implements the instance.Backend for instance.KindContainer.
package container

import (
	"context"
	"fmt"
	"slices"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostpath"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// Source is the subset of *store.Store's methods DockerBackend needs to
// resolve container registry mirrors.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
}

// DockerBackend implements instance.Backend against a real Docker daemon.
type DockerBackend struct {
	Client   *docker.Client
	Source   Source         // nil is fine, just means no mirrors ever get applied
	Registry InstanceLister // nil disables exit detection

	exits  exitWatch
	listed guestFileCache
}

var (
	_ instance.Backend = (*DockerBackend)(nil)
	_ ImageLister      = (*DockerBackend)(nil)
	_ ImageDeleter     = (*DockerBackend)(nil)
)

func NewDockerBackend(socket string, source Source) *DockerBackend {
	return &DockerBackend{Client: docker.NewClient(socket), Source: source}
}

// resolveImageRef rewrites ref through the highest-priority enabled
// container mirror configured for its upstream registry, if any.
func (b *DockerBackend) resolveImageRef(ref string) (string, error) {
	if b.Source == nil {
		return ref, nil
	}
	stored, err := b.Source.ListMirrors(store.MirrorKindContainer)
	if err != nil {
		return "", fmt.Errorf("docker: listing container mirrors: %w", err)
	}
	var mirrors []docker.RegistryMirror
	for _, m := range stored {
		if !m.Enabled || m.Registry == "" {
			continue
		}
		mirrors = append(mirrors, docker.RegistryMirror{Registry: m.Registry, MirrorOf: m.MirrorOf})
	}
	return docker.ResolveMirror(ref, mirrors), nil
}

// Create creates (but does not start) the container, recording its
// Docker-assigned ID on spec.Container.ContainerID.
func (b *DockerBackend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: Create called with a nil ContainerSpec")
	}
	c := spec.Container

	imageRef, err := b.resolveImageRef(c.ImageRef)
	if err != nil {
		return err
	}
	if imageRef != c.ImageRef && progress != nil {
		progress(fmt.Sprintf("using mirror: %s -> %s", c.ImageRef, imageRef))
	}

	if progress != nil {
		progress(fmt.Sprintf("checking for image %s", imageRef))
	}
	exists, err := b.Client.ImageExists(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("docker: checking for image %s: %w", imageRef, err)
	}
	if !exists {
		// Unlike `docker run`, container-create doesn't auto-pull a missing
		// image, so pull it explicitly and forward Docker's progress.
		if err := b.Client.PullImage(ctx, imageRef, progress); err != nil {
			return fmt.Errorf("docker: pulling %s: %w", imageRef, err)
		}
	}

	params := docker.CreateContainerParams{
		Name:         containerName(spec),
		Image:        imageRef,
		Env:          c.Env,
		Entrypoint:   c.Entrypoint,
		Cmd:          c.Cmd,
		NetworkMode:  c.NetworkMode,
		NetworkAlias: c.NetworkAlias,
		ExtraHosts:   c.ExtraHosts,
		DNSServers:   c.DNSServers,
		DNSSearch:    c.DNSSearch,
	}
	for _, v := range c.Volumes {
		params.Volumes = append(params.Volumes, docker.VolumeMount{
			HostPath:      v.HostPath,
			ContainerPath: v.ContainerPath,
			ReadOnly:      v.ReadOnly,
		})
	}
	if spec.GUI {
		addGUIParams(&params, spec.ID)
	}
	for _, p := range c.Ports {
		params.Ports = append(params.Ports, docker.PortMapping{
			HostPort:  p.HostPort,
			GuestPort: p.GuestPort,
			Protocol:  p.Protocol,
		})
	}

	id, err := b.Client.CreateContainer(ctx, params)
	if err != nil {
		return fmt.Errorf("docker: creating %s: %w", spec.Name, err)
	}
	c.ContainerID = id
	b.trackContainer(id, spec)
	return nil
}

// AddPort adds a published port to spec's container. Docker fixes port
// bindings at container-create time, so this recreates the whole container (restarting it if it was running) instead of just relaunching the instance.
func (b *DockerBackend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: AddPort called with a nil ContainerSpec")
	}
	proto := instance.Protocol(port.Protocol)
	for _, p := range spec.Container.Ports {
		if p.HostPort == port.HostPort && instance.Protocol(p.Protocol) == proto {
			return fmt.Errorf("docker: %s already publishes host port %d/%s", spec.Name, port.HostPort, proto)
		}
	}
	ports := append(slices.Clone(spec.Container.Ports), instance.PortMapping{
		HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: proto,
	})
	return b.recreate(ctx, spec, ports)
}

// RemovePort removes a published port previously added with AddPort or at
// launch, identified by hostPort/protocol. Recreates the container, same as AddPort.
func (b *DockerBackend) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: RemovePort called with a nil ContainerSpec")
	}
	proto := instance.Protocol(protocol)
	idx := -1
	for i, p := range spec.Container.Ports {
		if p.HostPort == hostPort && instance.Protocol(p.Protocol) == proto {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("docker: %s has no published port for host port %d/%s (currently exposed: %s)",
			spec.Name, hostPort, proto, instance.FormatPorts(spec.Container.Ports))
	}
	return b.recreate(ctx, spec, slices.Delete(slices.Clone(spec.Container.Ports), idx, idx+1))
}

// recreate rebuilds spec's container with ports, restarting it if it was running. On failure it
// rebuilds the old config; spec.Container.ContainerID always names the container that exists.
func (b *DockerBackend) recreate(ctx context.Context, spec *instance.Spec, ports []instance.PortMapping) error {
	state, err := b.Status(ctx, spec)
	if err != nil {
		return fmt.Errorf("docker: checking %s's state before recreating it: %w", spec.Name, err)
	}
	wasRunning := state == instance.StateRunning

	if wasRunning {
		if err := b.Stop(ctx, spec, false, 10*time.Second); err != nil {
			return fmt.Errorf("docker: stopping %s to recreate it: %w", spec.Name, err)
		}
	}
	if err := b.Delete(ctx, spec); err != nil {
		return fmt.Errorf("docker: removing %s's old container: %w", spec.Name, err)
	}
	spec.Container.ContainerID = ""
	old := spec.Container.Ports
	spec.Container.Ports = ports
	err = b.rebuild(ctx, spec, wasRunning)
	if err != nil {
		spec.Container.Ports = old
		if rerr := b.rebuild(ctx, spec, wasRunning); rerr != nil {
			return fmt.Errorf("%w (restoring the old container also failed: %v)", err, rerr)
		}
	}
	return err
}

// rebuild creates spec's container and starts it if start is set. On failure no container is left.
func (b *DockerBackend) rebuild(ctx context.Context, spec *instance.Spec, start bool) error {
	if err := b.Create(ctx, spec, nil); err != nil {
		return fmt.Errorf("docker: recreating %s: %w", spec.Name, err)
	}
	if start {
		if err := b.Start(ctx, spec); err != nil {
			_ = b.Client.RemoveContainer(ctx, spec.Container.ContainerID, true)
			spec.Container.ContainerID = ""
			return fmt.Errorf("docker: restarting %s after recreating it: %w", spec.Name, err)
		}
	}
	return nil
}

func containerName(spec *instance.Spec) string {
	return "anvil-" + spec.Name
}

func (b *DockerBackend) Start(ctx context.Context, spec *instance.Spec) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: Start called with a nil ContainerSpec")
	}
	if spec.Container.ContainerID == "" {
		return fmt.Errorf("docker: %s has no container ID yet (Create must run first)", spec.Name)
	}
	if spec.GUI {
		// /run is empty after a reboot; a missing bind source would make Docker create it root-owned.
		if _, err := ensureGUIDirs(spec.ID); err != nil {
			return err
		}
	}
	// Docker resolves bind sources again on every start, as root.
	for _, v := range spec.Container.Volumes {
		if err := hostpath.CheckResolved(v.HostPath); err != nil {
			return fmt.Errorf("docker: starting %s: volume %w", spec.Name, err)
		}
	}
	b.clearExpectedExit(spec.Container.ContainerID)
	if err := b.Client.StartContainer(ctx, spec.Container.ContainerID); err != nil {
		return fmt.Errorf("docker: starting %s: %w", spec.Name, err)
	}
	return nil
}

func (b *DockerBackend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: Stop called with a nil ContainerSpec")
	}
	if spec.Container.ContainerID == "" {
		return nil // never started, nothing to stop
	}
	timeoutSeconds := int(timeout.Seconds())
	if force {
		timeoutSeconds = 0
	}
	b.expectExit(spec.Container.ContainerID)
	if err := b.Client.StopContainer(ctx, spec.Container.ContainerID, timeoutSeconds); err != nil {
		b.clearExpectedExit(spec.Container.ContainerID) // a stale mark would hide a later crash
		return fmt.Errorf("docker: stopping %s: %w", spec.Name, err)
	}
	return nil
}

// Delete forcibly removes the container, stopping it first if still running.
func (b *DockerBackend) Delete(ctx context.Context, spec *instance.Spec) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: Delete called with a nil ContainerSpec")
	}
	if spec.Container.ContainerID == "" {
		return nil
	}
	b.expectExit(spec.Container.ContainerID)
	if err := b.Client.RemoveContainer(ctx, spec.Container.ContainerID, true); err != nil {
		b.clearExpectedExit(spec.Container.ContainerID)
		return fmt.Errorf("docker: removing %s: %w", spec.Name, err)
	}
	if spec.GUI {
		removeGUIDirs(spec.ID)
	}
	return nil
}

func (b *DockerBackend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	if spec.Container == nil || spec.Container.ContainerID == "" {
		return instance.StateStopped, nil
	}
	state, err := b.Client.InspectState(ctx, spec.Container.ContainerID)
	if err != nil {
		return instance.StateError, err
	}
	return dockerStatusToState(state), nil
}

func dockerStatusToState(s docker.ContainerState) instance.State {
	switch s.Status {
	case "created":
		return instance.StateStopped
	case "running":
		return instance.StateRunning
	case "restarting":
		return instance.StateStarting
	case "removing":
		return instance.StateDeleting
	case "paused", "exited", "dead":
		return instance.StateStopped
	default:
		if s.Running {
			return instance.StateRunning
		}
		return instance.StateStopped
	}
}

// Logs streams the container's stdout/stderr.
func (b *DockerBackend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: Logs called with a nil ContainerSpec")
	}
	if spec.Container.ContainerID == "" {
		return nil
	}
	return b.Client.Logs(ctx, spec.Container.ContainerID, follow, tailLines, send)
}

// statsSampleWindow bounds how long Stats waits between its two live
// samples when computing network/disk-IO rates.
const statsSampleWindow = 200 * time.Millisecond

var _ instance.StatsProvider = (*DockerBackend)(nil)

// Stats samples the container's live resource usage. CPU, network and disk-IO
// rates come from the delta between two one-shot samples statsSampleWindow apart.
func (b *DockerBackend) Stats(ctx context.Context, spec *instance.Spec) (instance.Stats, error) {
	if spec.Container == nil {
		return instance.Stats{}, fmt.Errorf("docker: Stats called with a nil ContainerSpec")
	}
	id := spec.Container.ContainerID
	if id == "" {
		return instance.Stats{}, fmt.Errorf("docker: %s isn't running", spec.Name)
	}

	s0, err := b.Client.Stats(ctx, id)
	if err != nil {
		return instance.Stats{}, err
	}
	select {
	case <-time.After(statsSampleWindow):
	case <-ctx.Done():
		return instance.Stats{}, ctx.Err()
	}
	s1, err := b.Client.Stats(ctx, id)
	if err != nil {
		return instance.Stats{}, err
	}
	elapsed := s1.At.Sub(s0.At).Seconds()

	var cpuPercent float64
	cpuDelta := float64(s1.CPUTotalUsageNanos - s0.CPUTotalUsageNanos)
	systemDelta := float64(s1.CPUSystemNanos - s0.CPUSystemNanos)
	if systemDelta > 0 {
		cpuPercent = (cpuDelta / systemDelta) * float64(s1.OnlineCPUs) * 100
	}

	var netRx, netTx, blkRead, blkWrite float64
	if elapsed > 0 {
		netRx = float64(s1.NetRxBytes-s0.NetRxBytes) / elapsed
		netTx = float64(s1.NetTxBytes-s0.NetTxBytes) / elapsed
		blkRead = float64(s1.BlkReadBytes-s0.BlkReadBytes) / elapsed
		blkWrite = float64(s1.BlkWriteBytes-s0.BlkWriteBytes) / elapsed
	}

	insp, err := b.Client.Inspect(ctx, id)
	if err != nil {
		return instance.Stats{}, err
	}
	var uptime int64
	if !insp.StartedAt.IsZero() {
		if d := time.Since(insp.StartedAt); d > 0 {
			uptime = int64(d.Seconds())
		}
	}

	return instance.Stats{
		CPUPercent:           cpuPercent,
		MemUsedBytes:         s1.MemUsedBytes,
		MemLimitBytes:        s1.MemLimitBytes,
		DiskReadBytesPerSec:  blkRead,
		DiskWriteBytesPerSec: blkWrite,
		NetAvailable:         true,
		NetRxBytesPerSec:     netRx,
		NetTxBytesPerSec:     netTx,
		UptimeSeconds:        uptime,
		Address:              insp.Address,
	}, nil
}

// ListImages returns every image in Docker's local store, each tagged
// with how many containers (running or not) currently reference it.
func (b *DockerBackend) ListImages(ctx context.Context) ([]ImageInfo, error) {
	images, err := b.Client.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	inUse, err := b.Client.ImagesInUse(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ImageInfo, len(images))
	for i, img := range images {
		out[i] = ImageInfo{
			ID:        img.ID,
			Engine:    instance.ContainerEngineDocker,
			RepoTags:  img.RepoTags,
			SizeBytes: img.SizeBytes,
			RefCount:  inUse[img.ID],
		}
	}
	return out, nil
}

// DeleteImage removes id from Docker's local image store.
func (b *DockerBackend) DeleteImage(ctx context.Context, id string, force bool) error {
	return b.Client.RemoveImage(ctx, id, force)
}
