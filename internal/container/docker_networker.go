package container

import (
	"context"

	"github.com/anvil-project/anvil/internal/container/docker"
)

// DockerNetworker implements internal/intent.Networker against a real
// Docker daemon.
type DockerNetworker struct {
	Client *docker.Client
}

func NewDockerNetworker(client *docker.Client) *DockerNetworker {
	return &DockerNetworker{Client: client}
}

// CreateNetwork creates name's bridge network if it doesn't already exist.
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

// RemoveNetwork deletes name's network.
func (n *DockerNetworker) RemoveNetwork(ctx context.Context, name string) error {
	return n.Client.RemoveNetwork(ctx, name)
}

// ContainerAddress returns containerID's assigned address on networkName.
func (n *DockerNetworker) ContainerAddress(ctx context.Context, networkName, containerID string) (string, error) {
	return n.Client.ContainerNetworkAddress(ctx, containerID, networkName)
}
