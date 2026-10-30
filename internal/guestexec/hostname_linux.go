//go:build linux

package guestexec

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// hostsFile is the guest's /etc/hosts, a variable for tests.
var hostsFile = "/etc/hosts"

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// setHostname renames the guest to name and moves its /etc/hosts entry along, so tools
// that look up their own hostname still resolve it.
func setHostname(name string) error {
	if !hostnameRe.MatchString(name) {
		return fmt.Errorf("invalid hostname %q", name)
	}
	old, _ := os.Hostname()
	if err := unix.Sethostname([]byte(name)); err != nil {
		return err
	}
	data, err := os.ReadFile(hostsFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(hostsFile, []byte(renameInHosts(string(data), old, name)), 0o644)
}

// renameInHosts replaces old with name wherever it is a host name in hosts, and adds a
// 127.0.1.1 entry for name when no line had it.
func renameInHosts(hosts, old, name string) string {
	lines := strings.SplitAfter(hosts, "\n")
	found := false
	for i, line := range lines {
		body, comment, _ := strings.Cut(line, "#")
		fields := strings.Fields(body)
		changed := false
		for j := 1; j < len(fields); j++ {
			if old != "" && (fields[j] == old || strings.HasPrefix(fields[j], old+".")) {
				fields[j] = name + strings.TrimPrefix(fields[j], old)
				changed = true
			}
			found = found || fields[j] == name
		}
		if changed {
			lines[i] = strings.Join(fields, " ")
			if strings.Contains(line, "#") {
				lines[i] += " #" + strings.TrimSuffix(comment, "\n")
			}
			if strings.HasSuffix(line, "\n") {
				lines[i] += "\n"
			}
		}
	}
	out := strings.Join(lines, "")
	if !found {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "127.0.1.1 " + name + "\n"
	}
	return out
}
