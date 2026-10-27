package commands

import (
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.WaitReady(cmd.Context(), &anvilv1.WaitReadyRequest{
					Name:           args[0],
					TimeoutSeconds: int32(timeout.Seconds()),
				})
				if err != nil {
					return err
				}
				return streamProgress(cmd.OutOrStdout(), stream, "Ready")
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "give up after this long")
	return cmd
}
