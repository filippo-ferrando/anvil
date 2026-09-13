package commands

import (
	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newStartCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>...",
		Short: "Start one or more stopped instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Start(cmd.Context(), &anvilv1.StartRequest{Names: args})
			return err
		},
	}
}

func newStopCommand(flags *globalFlags) *cobra.Command {
	var (
		force   bool
		timeout int64
	)
	cmd := &cobra.Command{
		Use:   "stop <name>...",
		Short: "Stop one or more running instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Stop(cmd.Context(), &anvilv1.StopRequest{
				Names:          args,
				Force:          force,
				TimeoutSeconds: timeout,
			})
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "skip graceful shutdown")
	cmd.Flags().Int64Var(&timeout, "timeout", 30, "seconds to wait for a graceful shutdown before escalating")
	return cmd
}
