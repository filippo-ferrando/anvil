package commands

import (
	"time"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newWaitCommand(flags *globalFlags) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <name>",
		Short: "Wait until a running instance finished its first-boot setup",
		Long: "Wait until a running instance finished its first-boot setup: cloud-init for a VM, " +
			"nothing for a container. Fails if cloud-init reports errors or --timeout passes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.WaitReady(cmd.Context(), &anvilv1.WaitReadyRequest{
				Name:           args[0],
				TimeoutSeconds: int32(timeout.Seconds()),
			})
			if err != nil {
				return err
			}
			return streamProgress(cmd.OutOrStdout(), stream, "Ready")
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "give up after this long")
	return cmd
}
