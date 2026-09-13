package commands

import (
	"fmt"
	"io"
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

// humanBytes renders a byte count like "1.2 GiB" — good enough for a CLI
// table, no need for a dependency over this.
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

// printInstanceTable is a minimal stdlib text/tabwriter table — a nice-to-
// have richer table (color, sorting) is a TUI/M7 concern, not the CLI's.
func printInstanceTable(w io.Writer, instances []*anvilv1.Instance) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tSTATE\tIMAGE")
	for _, inst := range instances {
		image := ""
		if inst.GetVm() != nil {
			image = inst.GetVm().GetImageRef()
		} else if inst.GetContainer() != nil {
			image = inst.GetContainer().GetImageRef()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", inst.GetName(), kindLabel(inst.GetKind()), stateLabel(inst.GetState()), image)
	}
	tw.Flush()
}
