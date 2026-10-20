package client

import (
	"fmt"
	"strconv"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// ParseEnv parses KEY=VALUE entries into a map, nil when there are none.
func ParseEnv(vars []string) (map[string]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(vars))
	for _, v := range vars {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("env %q must be in KEY=VALUE form", v)
		}
		env[k] = val
	}
	return env, nil
}

// ParseVolumes parses "<host-path>:<container-path>[:ro]" entries.
func ParseVolumes(vols []string) ([]*anvilv1.VolumeMount, error) {
	var out []*anvilv1.VolumeMount
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf(`volume %q must be "<host-path>:<container-path>[:ro]"`, v)
		}
		readOnly := false
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf(`volume %q: third part must be "ro"`, v)
			}
			readOnly = true
		}
		out = append(out, &anvilv1.VolumeMount{HostPath: parts[0], ContainerPath: parts[1], ReadOnly: readOnly})
	}
	return out, nil
}

// ParsePorts parses "<host>:<guest>[/tcp|udp]" entries.
func ParsePorts(specs []string) ([]*anvilv1.PortMapping, error) {
	var out []*anvilv1.PortMapping
	for _, s := range specs {
		p, err := ParsePortMapping(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ParsePortMapping parses "<host>:<guest>[/tcp|udp]".
func ParsePortMapping(s string) (*anvilv1.PortMapping, error) {
	spec, protocol, err := splitProtocol(s)
	if err != nil {
		return nil, err
	}
	hostStr, guestStr, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf(`port must be "<host>:<guest>[/tcp|udp]", got %q`, s)
	}
	hostPort, err := strconv.Atoi(hostStr)
	if err != nil {
		return nil, fmt.Errorf("invalid host port %q: %w", hostStr, err)
	}
	guestPort, err := strconv.Atoi(guestStr)
	if err != nil {
		return nil, fmt.Errorf("invalid guest port %q: %w", guestStr, err)
	}
	return &anvilv1.PortMapping{HostPort: int32(hostPort), GuestPort: int32(guestPort), Protocol: protocol}, nil
}

// ParseHostPort parses "<host>[/tcp|udp]".
func ParseHostPort(s string) (hostPort int, protocol string, err error) {
	spec, protocol, err := splitProtocol(s)
	if err != nil {
		return 0, "", err
	}
	hostPort, err = strconv.Atoi(spec)
	if err != nil {
		return 0, "", fmt.Errorf("invalid host port %q: %w", spec, err)
	}
	return hostPort, protocol, nil
}

// splitProtocol splits "<rest>[/tcp|udp]", validating an explicit protocol
// and defaulting to "" (which every consumer already treats as "tcp").
func splitProtocol(s string) (rest, protocol string, err error) {
	rest, protocol, ok := strings.Cut(s, "/")
	if !ok {
		return s, "", nil
	}
	protocol = strings.ToLower(protocol)
	if protocol != "tcp" && protocol != "udp" {
		return "", "", fmt.Errorf(`protocol must be "tcp" or "udp", got %q`, protocol)
	}
	return rest, protocol, nil
}
