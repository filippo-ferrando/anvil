package commands

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newExportCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export <name|intent> -o <bundle.tar.zst>",
		Short: "Package a single instance, or a whole intent, into a portable bundle",
		Long: "Package a single instance, or a whole intent as one group, into a single " +
			"tar.zst bundle: VM disks (as qcow2 diffs against their base image, not " +
			"flattened), container specs, bind-mounted volume contents, and cloud-init " +
			"configs. name is looked up as a single instance first, a whole intent second. " +
			"Every member is stopped for a consistent snapshot, then resumed to whatever " +
			"state it was in before. `anvil import` brings a bundle back up.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("-o/--output is required")
			}
			absOutput, err := filepath.Abs(output)
			if err != nil {
				return fmt.Errorf("resolving %s: %w", output, err)
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.Export.Export(cmd.Context(), &anvilv1.ExportRequest{Name: args[0], OutputPath: absOutput})
			if err != nil {
				return err
			}
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.ExportProgress_Status:
					fmt.Fprintln(cmd.OutOrStdout(), e.Status)
				case *anvilv1.ExportProgress_Error:
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.ExportProgress_Done:
					fmt.Fprintf(cmd.OutOrStdout(), "Exported: %s\n", e.Done)
				}
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "path to write the bundle to (required)")
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
			absBundle, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolving %s: %w", args[0], err)
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.Export.Import(cmd.Context(), &anvilv1.ImportRequest{BundlePath: absBundle, Name: name})
			if err != nil {
				return err
			}
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.ImportProgress_Status:
					fmt.Fprintln(cmd.OutOrStdout(), e.Status)
				case *anvilv1.ImportProgress_Error:
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.ImportProgress_Done:
					done := e.Done
					if done.GetIntentName() != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "Imported: intent %q\n", done.GetIntentName())
						for _, mr := range done.GetMembers() {
							fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", mr.GetRole(), mr.GetNewId())
						}
					} else if len(done.GetMembers()) > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "Imported: %s\n", done.GetMembers()[0].GetNewId())
					}
				}
			}
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "rename the imported intent, or the instance for a standalone bundle")
	return cmd
}
