package tui

import (
	"fmt"
	"strconv"
	"strings"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// The launch form's env/volumes/ports fields are each one comma-separated
// text field rather than a dynamic add/remove row list (a deliberate v1
// simplification, a real per-row editor is a nicer follow-up), using the
// same per-item formats internal/cli/commands/launch.go's own
// --env/--volume/--publish flags already use — a small, deliberate
// duplication: this package only reuses pkg/client, not
// internal/cli/commands, per the plan's TUI section.

func parseInt32(s, field string) (int32, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return int32(n), nil
}

func parseInt64(s, field string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return n, nil
}

func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseEnvField(s string) (map[string]string, error) {
	items := splitCommaList(s)
	if len(items) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(items))
	for _, v := range items {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("env %q must be in KEY=VALUE form", v)
		}
		env[k] = val
	}
	return env, nil
}

func parseVolumesField(s string) ([]*anvilv1.VolumeMount, error) {
	var out []*anvilv1.VolumeMount
	for _, v := range splitCommaList(s) {
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

func parsePortsField(s string) ([]*anvilv1.PortMapping, error) {
	var out []*anvilv1.PortMapping
	for _, p := range splitCommaList(s) {
		spec, protocol := p, "tcp"
		if host, proto, ok := strings.Cut(p, "/"); ok {
			spec, protocol = host, proto
		}
		hostStr, guestStr, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf(`port %q must be "<host-port>:<guest-port>[/tcp|udp]"`, p)
		}
		hostPort, err := strconv.Atoi(hostStr)
		if err != nil {
			return nil, fmt.Errorf("port %q: invalid host port: %w", p, err)
		}
		guestPort, err := strconv.Atoi(guestStr)
		if err != nil {
			return nil, fmt.Errorf("port %q: invalid guest port: %w", p, err)
		}
		out = append(out, &anvilv1.PortMapping{HostPort: int32(hostPort), GuestPort: int32(guestPort), Protocol: protocol})
	}
	return out, nil
}
