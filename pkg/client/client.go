// Package client is the thin gRPC client wrapper both cmd/anvil and (later)
// internal/tui build on — it holds no business logic, just a dial helper
// and the generated service client, per the plan's "clients are thin"
// mandate.
package client

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// Client embeds InstanceServiceClient anonymously so existing call sites
// (c.Launch, c.List, ...) keep working unqualified, but keeps CloudInit and
// Mirror as named fields rather than also embedding them — all three
// generated clients have a List method, and Go can't resolve an unqualified
// c.List if it's promoted from more than one embedded interface at the same
// depth.
type Client struct {
	conn *grpc.ClientConn
	anvilv1.InstanceServiceClient
	CloudInit anvilv1.CloudInitServiceClient
	Mirror    anvilv1.MirrorServiceClient
	Image     anvilv1.ImageServiceClient
	Intent    anvilv1.IntentServiceClient
	Host      anvilv1.HostServiceClient
	Migrate   anvilv1.MigrateServiceClient
}

// Dial connects to anvild's gRPC API over its unix socket at socketPath.
// No TLS/auth is configured — access control is the socket's filesystem
// permissions (the "anvil" group), per the plan.
func Dial(socketPath string) (*Client, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("client: dialing %s: %w", socketPath, err)
	}
	return &Client{
		conn:                  conn,
		InstanceServiceClient: anvilv1.NewInstanceServiceClient(conn),
		CloudInit:             anvilv1.NewCloudInitServiceClient(conn),
		Mirror:                anvilv1.NewMirrorServiceClient(conn),
		Image:                 anvilv1.NewImageServiceClient(conn),
		Intent:                anvilv1.NewIntentServiceClient(conn),
		Host:                  anvilv1.NewHostServiceClient(conn),
		Migrate:               anvilv1.NewMigrateServiceClient(conn),
	}, nil
}

func (c *Client) Close() error { return c.conn.Close() }
