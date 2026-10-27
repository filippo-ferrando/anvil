package commands

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newExportCommand(flags *globalFlags) *cobra.Command {
	var (
		output   string
		isIntent bool
	)
	cmd := &cobra.Command{
		Use:   "export <name|intent> -o <bundle.tar.zst>",
		Short: "Package a single instance, or a whole intent, into a portable bundle",
		Long: "Package a single instance, or a whole intent as one group, into a single " +
			"tar.zst bundle: VM disks (as qcow2 diffs against their base image, not " +
			"flattened), container specs, bind-mounted volume contents, and cloud-init " +
			"configs. name is looked up as a single instance first, a whole intent second (--intent skips the instance lookup). " +
			"Every member is stopped for a consistent snapshot, then resumed to whatever " +
			"state it was in before. `anvil import` brings a bundle back up.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("-o/--output is required")
			}
			return withClient(flags, func(c *client.Client) error {
				absOutput, err := c.HostPath(output)
				if err != nil {
					return err
				}

				stream, err := c.Export.Export(cmd.Context(), &anvilv1.ExportRequest{Name: args[0], OutputPath: absOutput, IsIntent: isIntent})
				if err != nil {
					return err
				}
				return streamStatus(cmd.OutOrStdout(), stream.Recv, func(l *liveLine, ev *anvilv1.ExportProgress) (bool, error) {
					switch e := ev.GetEvent().(type) {
					case *anvilv1.ExportProgress_Status:
						l.set(e.Status)
					case *anvilv1.ExportProgress_Error:
						return false, errors.New(e.Error)
					case *anvilv1.ExportProgress_Done:
						l.printf("Exported: %s\n", e.Done)
					}
					return false, nil
				})
			})
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "path to write the bundle to (required)")
	cmd.Flags().BoolVar(&isIntent, "intent", false, "treat the name as an intent, even if an instance has the same name")
	return cmd
}

func newImportCommand(flags *globalFlags) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "import <bundle.tar.zst>",
		Short: "Bring up the instance(s) packaged by `anvil export`",
		Long: "Relaunch every instance in a bundle produced by `anvil export`: VM disks " +
			"are rebased onto this host's own copy of their base image (downloaded first " +
			"if needed), and container volumes are restored to a new local directory. " +
			"`--name` renames the imported intent, or the instance for a standalone bundle.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				absBundle, err := c.HostPath(args[0])
				if err != nil {
					return err
				}

				stream, err := c.Export.Import(cmd.Context(), &anvilv1.ImportRequest{BundlePath: absBundle, Name: name})
				if err != nil {
					return err
				}
				return streamStatus(cmd.OutOrStdout(), stream.Recv, func(l *liveLine, ev *anvilv1.ImportProgress) (bool, error) {
					switch e := ev.GetEvent().(type) {
					case *anvilv1.ImportProgress_Status:
						l.set(e.Status)
					case *anvilv1.ImportProgress_Error:
						return false, errors.New(e.Error)
					case *anvilv1.ImportProgress_Done:
						done := e.Done
						if done.GetIntentName() != "" {
							l.printf("Imported: intent %q\n", done.GetIntentName())
							for _, mr := range done.GetMembers() {
								l.printf("  %s: %s\n", mr.GetRole(), mr.GetNewId())
							}
						} else if len(done.GetMembers()) > 0 {
							l.printf("Imported: %s\n", done.GetMembers()[0].GetNewId())
						}
					}
					return false, nil
				})
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "rename the imported intent, or the instance for a standalone bundle")
	return cmd
}
