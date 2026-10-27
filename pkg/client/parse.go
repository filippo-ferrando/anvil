package client

import (
	"fmt"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
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
		m, err := instance.ParseVolumeMount(v)
		if err != nil {
			return nil, err
		}
		out = append(out, &anvilv1.VolumeMount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly})
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
	p, err := instance.ParsePortMapping(s)
	if err != nil {
		return nil, err
	}
	return &anvilv1.PortMapping{HostPort: int32(p.HostPort), GuestPort: int32(p.GuestPort), Protocol: p.Protocol}, nil
}

// ParseHostPort parses "<host>[/tcp|udp]".
func ParseHostPort(s string) (hostPort int, protocol string, err error) {
	return instance.ParseHostPort(s)
}
