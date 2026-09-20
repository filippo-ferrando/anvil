package container

import (
	"context"
	"fmt"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
)

// Backend dispatches instance.KindContainer calls to the concrete
// per-engine backend selected by spec.Container.Engine.
type Backend struct {
	Docker instance.Backend // nil if Docker isn't configured
	Podman instance.Backend // nil until Podman lands
}

var (
	_ instance.Backend       = (*Backend)(nil)
	_ instance.StatsProvider = (*Backend)(nil)
)

func NewBackend(dockerBackend instance.Backend) *Backend {
	return &Backend{Docker: dockerBackend}
}

// engineFor picks the concrete backend for spec, defaulting an unset engine to Docker.
func (b *Backend) engineFor(spec *instance.Spec) (instance.Backend, error) {
	if spec.Container == nil {
		return nil, fmt.Errorf("container: spec has no ContainerSpec")
	}
	engine := spec.Container.Engine
	if engine == "" {
		engine = instance.ContainerEngineDocker
	}
	switch engine {
	case instance.ContainerEngineDocker:
		if b.Docker == nil {
			return nil, fmt.Errorf("container: the docker engine isn't configured on this daemon")
		}
		return b.Docker, nil
	case instance.ContainerEnginePodman:
		if b.Podman == nil {
			return nil, fmt.Errorf("container: podman support isn't implemented yet")
		}
		return b.Podman, nil
	default:
		return nil, fmt.Errorf("container: unknown engine %q", engine)
	}
}

func (b *Backend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	return eng.Create(ctx, spec, progress)
}

func (b *Backend) Start(ctx context.Context, spec *instance.Spec) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	return eng.Start(ctx, spec)
}

func (b *Backend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	return eng.Stop(ctx, spec, force, timeout)
}

func (b *Backend) Delete(ctx context.Context, spec *instance.Spec) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	return eng.Delete(ctx, spec)
}

func (b *Backend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	eng, err := b.engineFor(spec)
	if err != nil {
		return instance.StateError, err
	}
	return eng.Status(ctx, spec)
}

func (b *Backend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	return eng.Logs(ctx, spec, follow, tailLines, send)
}

func (b *Backend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	pf, ok := eng.(instance.PortForwarder)
	if !ok {
		return fmt.Errorf("container: this engine doesn't support port forwarding changes")
	}
	return pf.AddPort(ctx, spec, port)
}

func (b *Backend) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	eng, err := b.engineFor(spec)
	if err != nil {
		return err
	}
	pf, ok := eng.(instance.PortForwarder)
	if !ok {
		return fmt.Errorf("container: this engine doesn't support port forwarding changes")
	}
	return pf.RemovePort(ctx, spec, hostPort, protocol)
}

func (b *Backend) Stats(ctx context.Context, spec *instance.Spec) (instance.Stats, error) {
	eng, err := b.engineFor(spec)
	if err != nil {
		return instance.Stats{}, err
	}
	sp, ok := eng.(instance.StatsProvider)
	if !ok {
		return instance.Stats{}, fmt.Errorf("container: this engine doesn't support stats")
	}
	return sp.Stats(ctx, spec)
}

// ImageInfo is one image cached by a container engine: a separate
// inventory from the VM image vault (internal/vm/image.Vault).
type ImageInfo struct {
	ID        string
	Engine    instance.ContainerEngine
	RepoTags  []string
	SizeBytes int64
	RefCount  int
}

// ImageLister is implemented by an engine backend that can list its own
// cached images.
type ImageLister interface {
	ListImages(ctx context.Context) ([]ImageInfo, error)
}

// ImageDeleter is implemented by an engine backend that can delete one of
// its own cached images.
type ImageDeleter interface {
	DeleteImage(ctx context.Context, id string, force bool) error
}

// ListImages returns every image cached by every configured engine.
// An engine that can't list images just contributes nothing, rather than failing the whole call.
func (b *Backend) ListImages(ctx context.Context) ([]ImageInfo, error) {
	var all []ImageInfo
	for _, eng := range []instance.Backend{b.Docker, b.Podman} {
		lister, ok := eng.(ImageLister)
		if !ok {
			continue
		}
		images, err := lister.ListImages(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, images...)
	}
	return all, nil
}

// DeleteImage deletes id from engine's own image store.
func (b *Backend) DeleteImage(ctx context.Context, engine instance.ContainerEngine, id string, force bool) error {
	var eng instance.Backend
	switch engine {
	case instance.ContainerEngineDocker, "":
		eng = b.Docker
	case instance.ContainerEnginePodman:
		eng = b.Podman
	default:
		return fmt.Errorf("container: unknown engine %q", engine)
	}
	if eng == nil {
		return fmt.Errorf("container: the %s engine isn't configured on this daemon", engine)
	}
	deleter, ok := eng.(ImageDeleter)
	if !ok {
		return fmt.Errorf("container: this engine doesn't support deleting images")
	}
	return deleter.DeleteImage(ctx, id, force)
}
