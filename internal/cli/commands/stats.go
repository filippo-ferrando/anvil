package commands

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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
			return withClient(flags, func(c *client.Client) error {
				out := cmd.OutOrStdout()
				if !watch {
					reply, err := c.Stats(cmd.Context(), &anvilv1.StatsRequest{Name: args[0]})
					if err != nil {
						return err
					}
					printStats(out, args[0], reply.GetStats())
					return nil
				}

				clearScreen := isTerminalWriter(out)
				show := func(st *anvilv1.InstanceStats) {
					if clearScreen {
						fmt.Fprint(out, "\033[H\033[2J")
					}
					printStats(out, args[0], st)
				}
				stream, err := c.StreamStats(cmd.Context(), &anvilv1.StreamStatsRequest{Name: args[0], IntervalMs: int32(interval.Milliseconds())})
				for err == nil {
					var reply *anvilv1.StatsReply
					if reply, err = stream.Recv(); err == nil {
						show(reply.GetStats())
					}
				}
				if status.Code(err) == codes.Canceled {
					return nil
				}
				if err == io.EOF {
					fmt.Fprintf(out, "%s is not running\n", args[0])
					return nil
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "keep refreshing until interrupted, like docker stats")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval in --watch mode")
	return cmd
}

func printStats(w io.Writer, name string, s *anvilv1.InstanceStats) {
	fmt.Fprintf(w, "Name:\t%s\n", name)
	fmt.Fprintf(w, "Address:\t%s\n", orDash(s.GetAddress()))
	fmt.Fprintf(w, "Uptime:\t%s\n", format.Uptime(s.GetUptimeSeconds(), ""))
	fmt.Fprintf(w, "CPU:\t%.1f%%\n", s.GetCpuPercent())
	fmt.Fprintf(w, "Memory:\t%s / %s\n", progress.Bytes(s.GetMemUsedBytes()), progress.Bytes(s.GetMemLimitBytes()))
	if s.GetDiskTotalBytes() > 0 {
		fmt.Fprintf(w, "Disk:\t%s / %s\n", progress.Bytes(s.GetDiskUsedBytes()), progress.Bytes(s.GetDiskTotalBytes()))
	} else {
		fmt.Fprintf(w, "Disk:\tread %s/s, write %s/s\n",
			progress.Bytes(int64(s.GetDiskReadBytesPerSec())), progress.Bytes(int64(s.GetDiskWriteBytesPerSec())))
	}
	if s.GetNetAvailable() {
		fmt.Fprintf(w, "Network:\t↓ %s/s  ↑ %s/s\n",
			progress.Bytes(int64(s.GetNetRxBytesPerSec())), progress.Bytes(int64(s.GetNetTxBytesPerSec())))
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
