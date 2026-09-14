package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// NetworkCreateParams describes a bridge network to create.
type NetworkCreateParams struct {
	Name            string
	BridgeInterface string
	Subnet          string // CIDR, e.g. "10.55.201.0/24"
	Gateway         string // e.g. "10.55.201.1"
	IPRange         string // CIDR sub-range for Docker's IPAM to assign from, e.g. "10.55.201.128/25"
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

// CreateNetwork creates a new bridge network, returning its engine-assigned ID.
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
	resp, err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil)
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

type networkSummary struct {
	Name string `json:"Name"`
}

// ListNetworks returns the names of every network currently in Docker's local store.
func (c *Client) ListNetworks(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/networks", nil)
	if err != nil {
		return nil, err
	}
	var out []networkSummary
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return nil, fmt.Errorf("docker: listing networks: %w", err)
	}
	names := make([]string, len(out))
	for i, n := range out {
		names[i] = n.Name
	}
	return names, nil
}

// RemoveNetwork deletes a network. An already-gone network is not an error.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("docker: removing network %s: %w", name, err)
	}
	return nil
}
