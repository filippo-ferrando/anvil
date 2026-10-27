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
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newWatchCommand(flags *globalFlags) *cobra.Command {
	var existing bool
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Print instance changes as they happen",
		Long: "Print one line per instance change (created, started, stopped, deleted, ...) " +
			"until interrupted. --existing first prints one line per current instance.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Watch(cmd.Context(), &anvilv1.WatchRequest{IncludeExisting: existing})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				for {
					ev, err := stream.Recv()
					if err == io.EOF || status.Code(err) == codes.Canceled {
						return nil
					}
					if err != nil {
						return err
					}
					fmt.Fprintln(out, formatWatchEvent(time.Now(), ev))
				}
			})
		},
	}
	cmd.Flags().BoolVar(&existing, "existing", false, "print every current instance first")
	return cmd
}

// formatWatchEvent renders ev as "<time>  <name>  <kind>  <state or deleted>".
func formatWatchEvent(at time.Time, ev *anvilv1.WatchEvent) string {
	inst := ev.GetInstance()
	what := stateLabel(inst.GetState())
	if ev.GetType() == anvilv1.WatchEventType_WATCH_EVENT_TYPE_DELETED {
		what = "Removed"
	}
	return fmt.Sprintf("%s  %-20s  %-9s  %s", at.Format(time.TimeOnly), inst.GetName(), format.Kind(inst.GetKind()), what)
}
