package commands

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newImageCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage cached VM base images (the downloaded distro images instances are cloned from)",
	}
	cmd.AddCommand(newImageListCommand(flags), newImageDeleteCommand(flags))
	return cmd
}

func newImageListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List cached VM base images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Image.List(cmd.Context(), &anvilv1.ImageListRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tARCH\tSIZE\tIN USE\tPATH")
			for _, img := range reply.GetImages() {
				inUse := fmt.Sprintf("%d", img.GetRefCount())
				if img.GetRefCount() == 0 {
					inUse = "no"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					img.GetId(), img.GetArch(), humanBytes(img.GetSizeBytes()), inUse, img.GetPath())
			}
			return tw.Flush()
		},
	}
}

func newImageDeleteCommand(flags *globalFlags) *cobra.Command {
	var (
		arch  string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a cached VM base image",
		Long: "Delete a cached VM base image. Refuses to delete one still referenced by a " +
			"current instance's disk (that would corrupt it) unless --force is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Image.Delete(cmd.Context(), &anvilv1.ImageDeleteRequest{
				Id:    args[0],
				Arch:  arch,
				Force: force,
			})
			return err
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "", "architecture, if the id is ambiguous across more than one")
	cmd.Flags().BoolVar(&force, "force", false, "delete even if an instance's disk still depends on it (this will break that instance)")
	return cmd
}
