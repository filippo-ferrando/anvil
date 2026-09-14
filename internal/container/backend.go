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
