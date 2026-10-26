package client

import (
	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// ParseDevices checks --device values and returns them in the daemon's form.
func ParseDevices(devs []string) ([]string, error) {
	var out []string
	for _, d := range devs {
		norm, err := instance.ParseDevice(d)
		if err != nil {
			return nil, err
		}
		out = append(out, norm)
	}
	return out, nil
}

// DeviceStatus is "free", "in use by <name>" or "blocked: <reason>".
func DeviceStatus(d *anvilv1.HostDevice) string {
	switch {
	case d.GetInUseBy() != "":
		return "in use by " + d.GetInUseBy()
	case d.GetBlockedReason() != "":
		return "blocked: " + d.GetBlockedReason()
	}
	return "free"
}
