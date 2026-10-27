// Package format turns API values into the plain-text labels shown by both the CLI and
// the TUI, so the two clients print the same words. The TUI adds color on top.
package format

import (
	"fmt"
	"strings"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// Kind is "vm", "container" or "unknown".
func Kind(k anvilv1.Kind) string {
	switch k {
	case anvilv1.Kind_KIND_VM:
		return "vm"
	case anvilv1.Kind_KIND_CONTAINER:
		return "container"
	default:
		return "unknown"
	}
}

// Engine is the container engine name; unset means docker.
func Engine(e anvilv1.ContainerEngine) string {
	if e == anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN {
		return "podman"
	}
	return "docker"
}

// Hypervisor is the VM hypervisor name; unset means qemu.
func Hypervisor(h anvilv1.VmHypervisor) string {
	if h == anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER {
		return "firecracker"
	}
	return "qemu"
}

// State is the lower-case instance state, e.g. "running".
func State(s anvilv1.State) string {
	switch s {
	case anvilv1.State_STATE_STOPPED:
		return "stopped"
	case anvilv1.State_STATE_STARTING:
		return "starting"
	case anvilv1.State_STATE_RUNNING:
		return "running"
	case anvilv1.State_STATE_STOPPING:
		return "stopping"
	case anvilv1.State_STATE_DELETING:
		return "deleting"
	case anvilv1.State_STATE_DELETED:
		return "deleted"
	case anvilv1.State_STATE_ERROR:
		return "error"
	default:
		return "unknown"
	}
}

// CloudInit is the guest's cloud-init status, e.g. "done".
func CloudInit(s anvilv1.CloudInitStatus) string {
	switch s {
	case anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_RUNNING:
		return "running"
	case anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_DONE:
		return "done"
	case anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_ERROR:
		return "error"
	case anvilv1.CloudInitStatus_CLOUD_INIT_STATUS_DISABLED:
		return "disabled"
	default:
		return "unknown"
	}
}

// InstanceIP is a bridged VM's static IP (reachable from the host), else the guest
// agent's first report, else "".
func InstanceIP(inst *anvilv1.Instance) string {
	if ip := inst.GetVm().GetStaticIp(); ip != "" {
		ip, _, _ = strings.Cut(ip, "/")
		return ip
	}
	if ips := inst.GetGuest().GetIpAddresses(); len(ips) > 0 {
		ip, _, _ := strings.Cut(ips[0], "/")
		return ip
	}
	return ""
}

// ShortImageID drops a "sha256:" prefix and keeps 12 hex characters, like `docker images`.
func ShortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// RestartPolicy renders p the way --restart takes it, e.g. "on-failure:3".
func RestartPolicy(p *anvilv1.RestartPolicy) string {
	mode := p.GetMode()
	if mode == "" {
		mode = "no"
	}
	if p.GetMaxRetries() > 0 {
		return fmt.Sprintf("%s:%d", mode, p.GetMaxRetries())
	}
	return mode
}

// Schedule renders a snapshot schedule, e.g. "every 6h0m0s, keep 8, last <time>", with the
// last run in timeLayout. It is "" when there is no schedule.
func Schedule(s *anvilv1.SnapshotSchedule, timeLayout string) string {
	if s.GetEverySeconds() <= 0 {
		return ""
	}
	out := fmt.Sprintf("every %s, keep %d", time.Duration(s.GetEverySeconds())*time.Second, s.GetKeep())
	if s.GetLastRunUnix() > 0 {
		out += ", last " + time.Unix(s.GetLastRunUnix(), 0).Format(timeLayout)
	}
	return out
}

// Uptime renders a second count like "1d<sep>2h<sep>3m", or "-" when not positive.
func Uptime(seconds int64, sep string) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%s%dh%s%dm", days, sep, hours, sep, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh%s%dm", hours, sep, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%s%ds", minutes, sep, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}
