package commands

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newImageCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage cached VM base images and container images",
	}
	cmd.AddCommand(
		newImageListCommand(flags), newImageDeleteCommand(flags),
		newImageChecksumCommand(flags), newImageContainersCommand(flags),
	)
	return cmd
}

func newImageListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached VM base images, plus Firecracker's raw copies and kernels (see `image containers list` for container images)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Image.List(cmd.Context(), &anvilv1.ImageListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tARCH\tKIND\tSIZE\tIN USE\tPATH")
				for _, img := range reply.GetImages() {
					inUse := fmt.Sprintf("%d", img.GetRefCount())
					if img.GetRefCount() == 0 {
						inUse = "no"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						img.GetId(), img.GetArch(), img.GetKind(), progress.Bytes(img.GetSizeBytes()), inUse, img.GetPath())
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newImageDeleteCommand(flags *globalFlags) *cobra.Command {
	var (
		arch  string
		kind  string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a cached VM base image",
		Long: "Delete a cached VM base image, along with its Firecracker raw copy. Refuses to delete one still " +
			"referenced by a current instance's disk (that would corrupt it) unless --force is given. " +
			"A Firecracker raw copy or kernel can be deleted on its own; it comes back the next time it's needed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Image.Delete(cmd.Context(), &anvilv1.ImageDeleteRequest{
					Id:    args[0],
					Arch:  arch,
					Force: force,
					Kind:  kind,
				})
				return err
			})
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "", "architecture, if the id is ambiguous across more than one")
	cmd.Flags().StringVar(&kind, "kind", "", `only delete this kind: "base", "firecracker-raw" or "firecracker-kernel" (default: the base image first)`)
	cmd.Flags().BoolVar(&force, "force", false, "delete even if an instance's disk still depends on it (this will break that instance)")
	return cmd
}

func newImageChecksumCommand(flags *globalFlags) *cobra.Command {
	var arch string
	cmd := &cobra.Command{
		Use:   "checksum <id>",
		Short: "Print the SHA256 of a cached VM base image",
		Long: "Print the SHA256 of a cached VM base image. Fails if the image isn't cached " +
			"on this host; it is never downloaded by this command.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Image.Checksum(cmd.Context(), &anvilv1.ImageChecksumRequest{Id: args[0], Arch: arch})
				if err != nil {
					return err
				}
				if !reply.GetCached() {
					return fmt.Errorf("image %q is not cached on this host", args[0])
				}
				fmt.Fprintln(cmd.OutOrStdout(), reply.GetSha256())
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "", "image architecture (default x86_64)")
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
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List images cached by the container engine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Image.ListContainerImages(cmd.Context(), &anvilv1.ContainerImageListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
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
						format.ShortImageID(img.GetId()), format.Engine(img.GetEngine()), tag, progress.Bytes(img.GetSizeBytes()), inUse)
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
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
			return withClient(flags, func(c *client.Client) error {
				var pbEngine anvilv1.ContainerEngine
				if engine == "podman" {
					pbEngine = anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN
				}
				_, err := c.Image.DeleteContainerImage(cmd.Context(), &anvilv1.ContainerImageDeleteRequest{
					Id:     args[0],
					Engine: pbEngine,
					Force:  force,
				})
				return err
			})
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "docker", `container engine: "docker" or "podman"`)
	cmd.Flags().BoolVar(&force, "force", false, "delete even if a container still depends on it")
	return cmd
}
