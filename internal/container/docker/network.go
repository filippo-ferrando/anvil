package docker

import (
	"context"
	"fmt"
	"net/http"
)

// NetworkCreateParams describes a bridge network to create for one
// intent's shared network (see internal/intent). bridgeInterface is
// requested explicitly via the driver's own bridge.name option instead of
// letting Docker pick its own (undocumented, version-dependent) default
// name — this is what lets internal/vm/network attach a VM's tap device
// to a known, stable interface name rather than guessing at Docker's
// internal naming convention.
type NetworkCreateParams struct {
	Name            string
	BridgeInterface string
	Subnet          string // CIDR, e.g. "10.55.201.0/24"
	Gateway         string // e.g. "10.55.201.1"
	IPRange         string // CIDR sub-range Docker's own IPAM may assign from, e.g. "10.55.201.128/25" — reserves the rest for anvil's own static VM assignments, see internal/intent
}

type networkIPAMConfig struct {
	Subnet  string `json:"Subnet,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
	IPRange string `json:"IPRange,omitempty"`
}

type networkIPAM struct {
	Config []networkIPAMConfig `json:"Config,omitempty"`
}

type createNetworkRequest struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options,omitempty"`
	IPAM    networkIPAM       `json:"IPAM"`
}

type createNetworkResponse struct {
	ID string `json:"Id"`
}

// CreateNetwork creates a new bridge network, returning its engine-
// assigned ID. Docker rejects a duplicate name (see NetworkExists — check
// first) and a subnet that collides with another network on the host
// (the caller should be prepared to retry with a different subnet, see
// internal/intent's subnet allocation).
func (c *Client) CreateNetwork(ctx context.Context, p NetworkCreateParams) (string, error) {
	req := createNetworkRequest{
		Name:   p.Name,
		Driver: "bridge",
		Options: map[string]string{
			"com.docker.network.bridge.name": p.BridgeInterface,
		},
		IPAM: networkIPAM{Config: []networkIPAMConfig{{
			Subnet:  p.Subnet,
			Gateway: p.Gateway,
			IPRange: p.IPRange,
		}}},
	}
	resp, err := c.do(ctx, http.MethodPost, "/networks/create", req)
	if err != nil {
		return "", err
	}
	var out createNetworkResponse
	if err := decodeJSON(resp, &out, http.StatusCreated); err != nil {
		return "", fmt.Errorf("docker: creating network %s: %w", p.Name, err)
	}
	return out.ID, nil
}

// NetworkExists reports whether a network named name already exists.
func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/networks/"+name, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusError(resp)
	}
}

// RemoveNetwork deletes a network. An already-gone network is not an
// error, matching RemoveContainer's idempotency.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/networks/"+name, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("docker: removing network %s: %w", name, err)
	}
	return nil
}
