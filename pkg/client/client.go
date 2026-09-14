// Package client provides a gRPC client wrapper around anvild's generated
// service clients.
package client

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// Client wraps a gRPC connection to anvild and its generated service clients.
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
