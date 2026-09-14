package commands

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newImageCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage cached VM base images and container images",
	}
	cmd.AddCommand(
		newImageListCommand(flags), newImageDeleteCommand(flags),
		newImageContainersCommand(flags),
	)
	return cmd
}

func newImageListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List cached VM base images (see `image containers list` for container images)",
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

func newImageContainersCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "containers",
		Short: "Manage images cached by the container engine (Docker today), separate from VM images",
	}
	cmd.AddCommand(newImageContainersListCommand(flags), newImageContainersDeleteCommand(flags))
	return cmd
}

func newImageContainersListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List images cached by the container engine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Image.ListContainerImages(cmd.Context(), &anvilv1.ContainerImageListRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tENGINE\tREPO:TAG\tSIZE\tIN USE")
			for _, img := range reply.GetImages() {
				inUse := fmt.Sprintf("%d", img.GetRefCount())
				if img.GetRefCount() == 0 {
					inUse = "no"
				}
				tag := "<none>"
				if tags := img.GetRepoTags(); len(tags) > 0 {
					tag = tags[0]
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					shortImageIDCLI(img.GetId()), engineLabel(img.GetEngine()), tag, humanBytes(img.GetSizeBytes()), inUse)
			}
			return tw.Flush()
		},
	}
}

func newImageContainersDeleteCommand(flags *globalFlags) *cobra.Command {
	var (
		engine string
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an image from the container engine's local store",
		Long: "Delete an image from the container engine's local store. Refuses to delete " +
			"one still referenced by any container (running or not) unless --force is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			var pbEngine anvilv1.ContainerEngine
			if engine == "podman" {
				pbEngine = anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN
			}
			_, err = c.Image.DeleteContainerImage(cmd.Context(), &anvilv1.ContainerImageDeleteRequest{
				Id:     args[0],
				Engine: pbEngine,
				Force:  force,
			})
			return err
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "docker", `container engine: "docker" or "podman"`)
	cmd.Flags().BoolVar(&force, "force", false, "delete even if a container still depends on it")
	return cmd
}

// shortImageIDCLI trims a "sha256:" prefix and shortens to 12 hex
// characters, matching `docker images`' own convention.
func shortImageIDCLI(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}
