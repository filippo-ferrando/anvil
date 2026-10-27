package commands

import (
	"context"
	"fmt"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// resolveInstance looks up name via the daemon's Info RPC.
func resolveInstance(ctx context.Context, c *client.Client, name string) (*anvilv1.Instance, error) {
	reply, err := c.Info(ctx, &anvilv1.InfoRequest{Names: []string{name}})
	if err != nil {
		return nil, err
	}
	instances := reply.GetInstances()
	if len(instances) == 0 {
		return nil, fmt.Errorf("no such instance %q", name)
	}
	return instances[0], nil
}
