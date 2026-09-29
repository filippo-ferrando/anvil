package commands

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func kindLabel(k anvilv1.Kind) string {
	switch k {
	case anvilv1.Kind_KIND_VM:
		return "vm"
	case anvilv1.Kind_KIND_CONTAINER:
		return "container"
	default:
		return "unknown"
	}
}

func engineLabel(e anvilv1.ContainerEngine) string {
	switch e {
	case anvilv1.ContainerEngine_CONTAINER_ENGINE_DOCKER:
		return "docker"
	case anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN:
		return "podman"
	default:
		return "docker" // unset defaults to docker
	}
}

func stateLabel(s anvilv1.State) string {
	switch s {
	case anvilv1.State_STATE_STOPPED:
		return "Stopped"
	case anvilv1.State_STATE_STARTING:
		return "Starting"
	case anvilv1.State_STATE_RUNNING:
		return "Running"
	case anvilv1.State_STATE_STOPPING:
		return "Stopping"
	case anvilv1.State_STATE_DELETING:
		return "Deleting"
	case anvilv1.State_STATE_DELETED:
		return "Deleted"
	case anvilv1.State_STATE_ERROR:
		return "Error"
	default:
		return "Unknown"
	}
}

// humanBytes renders a byte count as a human-readable string, e.g. "1.2 GiB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// instanceIP returns inst's main address for display: a bridged VM's static IP
// (reachable from the host), else the guest agent's first report, else "-".
func instanceIP(inst *anvilv1.Instance) string {
	if ip := inst.GetVm().GetStaticIp(); ip != "" {
		ip, _, _ = strings.Cut(ip, "/")
		return ip
	}
	if ips := inst.GetGuest().GetIpAddresses(); len(ips) > 0 {
		ip, _, _ := strings.Cut(ips[0], "/")
		return ip
	}
	return "-"
}

func cloudInitLabel(s anvilv1.CloudInitStatus) string {
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

// printInstanceTable prints instances as a tab-aligned table to w.
func printInstanceTable(w io.Writer, instances []*anvilv1.Instance) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tENGINE\tSTATE\tIP\tIMAGE")
	for _, inst := range instances {
		image := ""
		engine := "-"
		if inst.GetVm() != nil {
			image = inst.GetVm().GetImageRef()
		} else if c := inst.GetContainer(); c != nil {
			image = c.GetImageRef()
			engine = engineLabel(c.GetEngine())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", inst.GetName(), kindLabel(inst.GetKind()), engine, stateLabel(inst.GetState()), instanceIP(inst), image)
	}
	tw.Flush()
}
