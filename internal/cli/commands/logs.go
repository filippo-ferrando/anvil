package commands

import (
	"io"
	"os"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newLogsCommand(flags *globalFlags) *cobra.Command {
	var (
		follow bool
		tail   int32
	)
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show an instance's log output",
		Long: "Show an instance's log output. For a VM this is boot/console output (what " +
			"cloud-init prints while it runs), not application logs from inside the guest OS. " +
			"See `anvil shell`/`anvil exec` for that. Once M3 lands, a container's logs are its " +
			"actual stdout/stderr instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.Logs(cmd.Context(), &anvilv1.LogsRequest{
				Name:      args[0],
				Follow:    follow,
				TailLines: tail,
			})
			if err != nil {
				return err
			}
			for {
				chunk, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				os.Stdout.Write(chunk.GetData())
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new output as it arrives")
	cmd.Flags().Int32Var(&tail, "tail", 0, "only show the last N lines (0 = from the beginning)")
	return cmd
}
