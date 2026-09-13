// Package container is the instance.Backend for instance.KindContainer —
// see backend.go for how it dispatches between engines (Docker landed
// first, Podman is next) by spec.Container.Engine. This package (not
// internal/container/docker itself) is what depends on internal/instance:
// keeping internal/container/docker to just the REST client, with no
// dependency on the domain model, is what makes that client testable
// against a mock server independent of anything else in the daemon
// (see internal/vm/qemu vs internal/vm for the same split, done for the
// same reason).
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
// resolve container registry mirrors — same narrow-interface pattern as
// internal/vm.Backend's own Source, and for the same reason: it's obvious
// at a glance what this backend actually reads from the registry.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
}

// DockerBackend implements instance.Backend against a real Docker daemon.
// Unlike internal/vm.Backend, there's no in-process "is it running" state
// to lose on a daemon restart and no Reconciler to implement: Docker
// itself persists container objects independent of anvild, so Status just
// asks Docker fresh every time instead of trusting anything cached here.
type DockerBackend struct {
	Client *docker.Client
	Source Source // nil is fine, just means no mirrors ever get applied
}

var _ instance.Backend = (*DockerBackend)(nil)

func NewDockerBackend(socket string, source Source) *DockerBackend {
	return &DockerBackend{Client: docker.NewClient(socket), Source: source}
}

// resolveImageRef rewrites ref through the highest-priority enabled
// `--kind container` mirror configured for its upstream registry, if any
// — see docker.ResolveMirror's doc comment for why this is a client-side
// rewrite rather than reconfiguring dockerd itself.
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
// Docker-assigned ID on spec.Container.ContainerID — every later call
// (Start/Stop/Status/Logs/Delete) is by that ID, not by anvil's own
// instance ID or name.
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
		// Unlike `docker run`, a plain container-create doesn't auto-pull a
		// missing image — it just 404s (the real bug this fixes, caught by
		// filippo's own `anvil launch --kind container nginx:alpine` on a
		// real Docker daemon, no cached image locally). Docker's own pull
		// progress (per-layer download/extract status) is forwarded as-is,
		// see docker.Client.PullImage's own throttling — this is exactly
		// the "is it stuck or just slow" visibility that was missing.
		if err := b.Client.PullImage(ctx, imageRef, progress); err != nil {
			return fmt.Errorf("docker: pulling %s: %w", imageRef, err)
		}
	}

	params := docker.CreateContainerParams{
		Name:        containerName(spec),
		Image:       imageRef,
		Env:         c.Env,
		Entrypoint:  c.Entrypoint,
		Cmd:         c.Cmd,
		NetworkMode: c.NetworkMode,
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

// containerName gives Docker a name derived from anvil's own instance
// name, so `docker ps` is at least somewhat legible on its own — prefixed
// to avoid colliding with an unrelated container of the same name a user
// created outside anvil entirely.
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

// Delete removes the container outright (force, so a still-running one is
// stopped first rather than requiring a separate Stop call) — matching
// the VM backend's Delete, which also tears down unconditionally.
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

// Logs streams the container's actual stdout/stderr — unlike a VM's Logs
// (boot/console output only), this really is the application's own
// output, since Docker already captures it directly.
func (b *DockerBackend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	if spec.Container == nil {
		return fmt.Errorf("docker: Logs called with a nil ContainerSpec")
	}
	if spec.Container.ContainerID == "" {
		return nil
	}
	return b.Client.Logs(ctx, spec.Container.ContainerID, follow, tailLines, send)
}
