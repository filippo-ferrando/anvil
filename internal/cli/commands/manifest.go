package commands

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/apply"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/vars"
	"git.fferrando.cc/rdfilippo/anvil/internal/yamlcheck"
)

func newManifestCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Manage the saved intent manifest library",
		Long:  "Save anvil.yaml manifests on the daemon by name. Apply one with `anvil apply --saved <name>`.",
	}
	cmd.AddCommand(
		newManifestListCommand(flags),
		newManifestNewCommand(flags),
		newManifestEditCommand(flags),
		newManifestShowCommand(flags),
		newManifestRenameCommand(flags),
		newManifestDeleteCommand(flags),
		newValidateCommand(flags, "manifest", func(ctx context.Context, flags *globalFlags, name string) (string, error) {
			c, err := dial(flags)
			if err != nil {
				return "", err
			}
			defer c.Close()
			reply, err := c.Manifest.Get(ctx, &anvilv1.ManifestGetRequest{Name: name})
			return reply.GetContent(), err
		}, func(content string) []yamlcheck.Problem {
			values, err := vars.Load("", "", []byte(content), nil)
			if err != nil {
				return []yamlcheck.Problem{{Message: err.Error()}}
			}
			return apply.Check([]byte(content), values)
		}),
	)
	return cmd
}

func newManifestListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List saved manifests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Manifest.List(cmd.Context(), &anvilv1.ManifestListRequest{})
			if err != nil {
				return err
			}
			if output == "json" {
				return printJSON(cmd.OutOrStdout(), reply)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tINTENT\tMODIFIED")
			for _, m := range reply.GetManifests() {
				modified := time.Unix(m.GetModifiedAtUnix(), 0).Format(time.RFC3339)
				fmt.Fprintf(tw, "%s\t%s\t%s\n", m.GetName(), m.GetIntent(), modified)
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newManifestNewCommand(flags *globalFlags) *cobra.Command {
	var fromFile string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Save a new manifest, from a file or a starter template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			content := apply.Starter(args[0])
			if fromFile != "" {
				data, err := readManifest(fromFile)
				if err != nil {
					return err
				}
				content = string(data)
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Manifest.Save(cmd.Context(), &anvilv1.ManifestSaveRequest{Name: args[0], Content: content})
			return err
		},
	}
	cmd.Flags().StringVar(&fromFile, "from", "", `import an existing anvil.yaml, or "-" for stdin`)
	return cmd
}

func newManifestEditCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit a saved manifest in $EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			current := apply.Starter(name)
			existing, err := c.Manifest.Get(cmd.Context(), &anvilv1.ManifestGetRequest{Name: name})
			switch {
			case err == nil:
				current = existing.GetContent()
			case status.Code(err) == codes.NotFound:
			default:
				return fmt.Errorf("manifest: loading %q before edit: %w", name, err)
			}
			edited, err := editInEditor(current)
			if err != nil {
				return err
			}
			_, err = c.Manifest.Save(cmd.Context(), &anvilv1.ManifestSaveRequest{Name: name, Content: edited})
			return err
		},
	}
}

func newManifestShowCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print a saved manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			reply, err := c.Manifest.Get(cmd.Context(), &anvilv1.ManifestGetRequest{Name: args[0]})
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), reply.GetContent())
			return nil
		},
	}
}

func newManifestRenameCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old-name> <new-name>",
		Short: "Rename a saved manifest",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Manifest.Rename(cmd.Context(), &anvilv1.ManifestRenameRequest{OldName: args[0], NewName: args[1]})
			return err
		},
	}
}

func newManifestDeleteCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a saved manifest (its intent, if applied, is left running)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Manifest.Delete(cmd.Context(), &anvilv1.ManifestDeleteRequest{Name: args[0]})
			return err
		},
	}
}
