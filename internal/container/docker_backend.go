// Package container implements the instance.Backend for instance.KindContainer.
package container

import (
	"context"
	"fmt"
	"time"

	"github.com/anvil-project/anvil/internal/container/docker"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// Source is the subset of *store.Store's methods DockerBackend needs to
// resolve container registry mirrors.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
}

// DockerBackend implements instance.Backend against a real Docker daemon.
type DockerBackend struct {
	Client *docker.Client
	Source Source // nil is fine, just means no mirrors ever get applied
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
	}
	for _, v := range c.Volumes {
		params.Volumes = append(params.Volumes, docker.VolumeMount{
			HostPath:      v.HostPath,
			ContainerPath: v.ContainerPath,
			ReadOnly:      v.ReadOnly,
		})
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
	return nil
}

// containerName derives a Docker container name from anvil's instance name.
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
	if err := b.Client.StopContainer(ctx, spec.Container.ContainerID, timeoutSeconds); err != nil {
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
	if err := b.Client.RemoveContainer(ctx, spec.Container.ContainerID, true); err != nil {
		return fmt.Errorf("docker: removing %s: %w", spec.Name, err)
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

// Stats samples the container's live resource usage: CPU comes from
// Docker's own cpu_stats/precpu_stats pairing on the second sample;
// network/disk-IO throughput are computed from the delta between two
// samples statsSampleWindow apart.
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
