package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
)

func newVersionCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the anvil and anvild versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, commit := version.Get()
			fmt.Fprintf(cmd.OutOrStdout(), "anvil   %s\n", formatVersion(v, commit))

			// The daemon line is best-effort: the client version alone is still worth printing.
			c, err := dial(flags)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "anvild  unreachable: %v\n", err)
				return nil
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			reply, err := c.Version(ctx, &anvilv1.VersionRequest{})
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "anvild  unreachable: %v\n", err)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "anvild  %s\n", formatVersion(reply.GetVersion(), reply.GetCommit()))
			return nil
		},
	}
}

func formatVersion(v, commit string) string {
	if commit == "" {
		return v
	}
	return fmt.Sprintf("%s (%s)", v, commit)
}
