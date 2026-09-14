package commands

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newStatsCommand(flags *globalFlags) *cobra.Command {
	var watch bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "stats <name>",
		Short: "Show an instance's live CPU/memory/disk/network usage",
		Long: "Show a live resource-usage snapshot for a running instance: CPU (normalized to " +
			"its own CPU allocation), memory, disk, network throughput, uptime, and address. " +
			"Each snapshot takes a short live sample (a couple hundred ms) to compute a real rate.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			out := cmd.OutOrStdout()
			clearScreen := watch && isTerminalWriter(out)
			for {
				reply, err := c.Stats(cmd.Context(), &anvilv1.StatsRequest{Name: args[0]})
				if err != nil {
					return err
				}
				if clearScreen {
					fmt.Fprint(out, "\033[H\033[2J")
				}
				printStats(out, args[0], reply.GetStats())
				if !watch {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "keep refreshing until interrupted, like `docker stats`")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval in --watch mode")
	return cmd
}

func printStats(w io.Writer, name string, s *anvilv1.InstanceStats) {
	fmt.Fprintf(w, "Name:\t%s\n", name)
	fmt.Fprintf(w, "Address:\t%s\n", orDash(s.GetAddress()))
	fmt.Fprintf(w, "Uptime:\t%s\n", humanDuration(s.GetUptimeSeconds()))
	fmt.Fprintf(w, "CPU:\t%.1f%%\n", s.GetCpuPercent())
	fmt.Fprintf(w, "Memory:\t%s / %s\n", humanBytes(s.GetMemUsedBytes()), humanBytes(s.GetMemLimitBytes()))
	if s.GetDiskTotalBytes() > 0 {
		fmt.Fprintf(w, "Disk:\t%s / %s\n", humanBytes(s.GetDiskUsedBytes()), humanBytes(s.GetDiskTotalBytes()))
	} else {
		fmt.Fprintf(w, "Disk:\tread %s/s, write %s/s\n",
			humanBytes(int64(s.GetDiskReadBytesPerSec())), humanBytes(int64(s.GetDiskWriteBytesPerSec())))
	}
	if s.GetNetAvailable() {
		fmt.Fprintf(w, "Network:\t↓ %s/s  ↑ %s/s\n",
			humanBytes(int64(s.GetNetRxBytesPerSec())), humanBytes(int64(s.GetNetTxBytesPerSec())))
	} else {
		fmt.Fprintln(w, "Network:\tnot available")
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// humanDuration renders a second count as a compact "1d2h3m"-shaped uptime.
func humanDuration(seconds int64) string {
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
		return fmt.Sprintf("%dd%dh%dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}
