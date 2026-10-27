package commands

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newEventsCommand(flags *globalFlags) *cobra.Command {
	var follow bool
	var limit int32
	var output string
	cmd := &cobra.Command{
		Use:   "events [name]",
		Short: "Show the event log: launches, starts, stops, exits, restarts, changes",
		Long: "Show what happened to instances, oldest first: operations run through anvil (with the " +
			"user who ran them and any error), exits and starts outside anvil, and the daemon's own " +
			"restarts, autostarts and scheduled snapshots. The daemon keeps the newest 10000 events. " +
			"--follow keeps printing new events until interrupted.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				req := &anvilv1.EventsRequest{Limit: limit, Follow: follow}
				if len(args) == 1 {
					req.Name = args[0]
				}
				stream, err := c.Events(cmd.Context(), req)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if output == "table" {
					fmt.Fprintln(out, formatEventHeader())
				}
				for {
					ev, err := stream.Recv()
					if err == io.EOF || status.Code(err) == codes.Canceled {
						return nil
					}
					if err != nil {
						return err
					}
					if output == "json" {
						if err := printJSON(out, ev); err != nil {
							return err
						}
						continue
					}
					fmt.Fprintln(out, formatEvent(ev))
				}
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events until interrupted")
	cmd.Flags().Int32VarP(&limit, "limit", "n", 100, "print only the newest n stored events, 0 for all")
	addOutputFlag(cmd, &output)
	return cmd
}

const eventLineFormat = "%-19s  %-20s  %-16s  %-12s  %s"

func formatEventHeader() string {
	return fmt.Sprintf(eventLineFormat, "TIME", "INSTANCE", "ACTION", "ACTOR", "DETAILS")
}

// formatEvent renders ev as one fixed-width line, so --follow output stays aligned.
func formatEvent(ev *anvilv1.InstanceEvent) string {
	details := ev.GetMessage()
	if e := ev.GetError(); e != "" {
		details = strings.TrimSpace(details + "  failed: " + e)
	}
	at := time.Unix(ev.GetTimeUnix(), 0).Format(time.DateTime)
	return fmt.Sprintf(eventLineFormat, at, ev.GetInstanceName(), ev.GetAction(), ev.GetActor(), details)
}
