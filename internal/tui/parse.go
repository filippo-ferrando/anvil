package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
)

// appendProgressLine appends status to lines, replacing the last line instead
// of adding a new one when it shares the same progress.Key.
func appendProgressLine(lines []string, status string) []string {
	if len(lines) > 0 && progress.Key(lines[len(lines)-1]) == progress.Key(status) {
		lines[len(lines)-1] = status
		return lines
	}
	return append(lines, status)
}

// stepStart returns since when status continues the last line's step, else now: the start
// of the step that progressView times. Call it before appendProgressLine.
func stepStart(lines []string, status string, since time.Time) time.Time {
	if len(lines) > 0 && progress.Key(lines[len(lines)-1]) == progress.Key(status) {
		return since
	}
	return time.Now()
}

// progressView renders a transcript with a bar on every percentage. While running, the last
// line also shows a spinner and its step's time, redrawn by the one-second tick.
func progressView(lines []string, running bool, since time.Time) string {
	var b strings.Builder
	for i, line := range lines {
		if running && i == len(lines)-1 {
			elapsed := time.Since(since)
			line = progress.Live(line, elapsed, int(elapsed.Seconds()))
		} else {
			line = progress.Bar(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// The launch form's env/volumes/ports fields are each one comma-separated text field.

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

// humanBytesTUI renders n as a short, human-readable size (KiB/MiB/...).
func humanBytesTUI(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
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
