package container

import (
	"context"

	"github.com/anvil-project/anvil/internal/container/docker"
)

// DockerNetworker implements internal/intent.Networker against a real
// Docker daemon — the intent-network equivalent of DockerBackend.
type DockerNetworker struct {
	Client *docker.Client
}

func NewDockerNetworker(client *docker.Client) *DockerNetworker {
	return &DockerNetworker{Client: client}
}

// CreateNetwork creates name's bridge network if it doesn't already exist.
// Idempotent by design (checked via NetworkExists first) since
// intent.Manager.ensureNetwork only calls this once per intent in the
// normal case, but a daemon restart between creating the network and
// persisting store.Intent.Network would otherwise make the second attempt
// fail outright.
func (n *DockerNetworker) CreateNetwork(ctx context.Context, name, bridgeInterface, subnet, gateway, dockerIPRange string) error {
	exists, err := n.Client.NetworkExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = n.Client.CreateNetwork(ctx, docker.NetworkCreateParams{
		Name:            name,
		BridgeInterface: bridgeInterface,
		Subnet:          subnet,
		Gateway:         gateway,
		IPRange:         dockerIPRange,
	})
	return err
}

// RemoveNetwork deletes name's network. Docker refuses to remove a
// network that still has containers attached (an "active endpoints"
// error) — that's surfaced as a plain error here, not swallowed;
// intent.Manager.Delete is what decides that's non-fatal to the overall
// intent deletion, not this type.
func (n *DockerNetworker) RemoveNetwork(ctx context.Context, name string) error {
	return n.Client.RemoveNetwork(ctx, name)
}
