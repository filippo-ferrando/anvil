package commands

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newCloudInitCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-init",
		Short: "Manage the saved cloud-init user-data library",
	}
	cmd.AddCommand(
		newCloudInitListCommand(flags),
		newCloudInitNewCommand(flags),
		newCloudInitEditCommand(flags),
		newCloudInitShowCommand(flags),
		newCloudInitRenameCommand(flags),
		newCloudInitDeleteCommand(flags),
		newCloudInitImportRepoCommand(flags),
	)
	return cmd
}

func newCloudInitListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved cloud-init configs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.CloudInit.List(cmd.Context(), &anvilv1.CloudInitListRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMODIFIED")
			for _, cfg := range reply.GetConfigs() {
				modified := time.Unix(cfg.GetModifiedAtUnix(), 0).Format(time.RFC3339)
				fmt.Fprintf(tw, "%s\t%s\n", cfg.GetName(), modified)
			}
			return tw.Flush()
		},
	}
}

func newCloudInitNewCommand(flags *globalFlags) *cobra.Command {
	var fromFile string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new saved cloud-init config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			content := "#cloud-config\n"
			if fromFile != "" {
				data, err := readCloudInitFile(fromFile)
				if err != nil {
					return err
				}
				content = data
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.CloudInit.Save(cmd.Context(), &anvilv1.CloudInitSaveRequest{Name: name, Content: content})
			return err
		},
	}
	cmd.Flags().StringVar(&fromFile, "from", "", `import content from a file, or "-" for stdin (defaults to an empty cloud-config)`)
	return cmd
}

func newCloudInitEditCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit a saved cloud-init config in $EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			current := "#cloud-config\n"
			existing, err := c.CloudInit.Get(cmd.Context(), &anvilv1.CloudInitGetRequest{Name: name})
			switch {
			case err == nil:
				current = existing.GetContent()
			case status.Code(err) == codes.NotFound:
			default:
				return fmt.Errorf("cloud-init: loading %q before edit: %w", name, err)
			}

			edited, err := editInEditor(current)
			if err != nil {
				return err
			}

			_, err = c.CloudInit.Save(cmd.Context(), &anvilv1.CloudInitSaveRequest{Name: name, Content: edited})
			return err
		},
	}
}

func newCloudInitShowCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print a saved cloud-init config's content",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			reply, err := c.CloudInit.Get(cmd.Context(), &anvilv1.CloudInitGetRequest{Name: args[0]})
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), reply.GetContent())
			return nil
		},
	}
}

func newCloudInitRenameCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old-name> <new-name>",
		Short: "Rename a saved cloud-init config",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.CloudInit.Rename(cmd.Context(), &anvilv1.CloudInitRenameRequest{OldName: args[0], NewName: args[1]})
			return err
		},
	}
}

func newCloudInitDeleteCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a saved cloud-init config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.CloudInit.Delete(cmd.Context(), &anvilv1.CloudInitDeleteRequest{Name: args[0]})
			return err
		},
	}
}

// newCloudInitImportRepoCommand bulk-imports cloud-init templates from a repo
// manifest into the saved library.
func newCloudInitImportRepoCommand(flags *globalFlags) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "import-repo <manifest-url>",
		Short: "Bulk-import cloud-init templates from a repo manifest",
		Long: "Fetch a repo manifest and save every template it lists into the saved " +
			"library. A name that's already in the library is left alone unless " +
			"--force is given; a template that fails to fetch or save doesn't stop " +
			"the rest of the repo from importing. See docs/mirrors.md for the " +
			"manifest shape and how to host a repo of your own.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.CloudInit.ImportRepo(cmd.Context(), &anvilv1.CloudInitImportRepoRequest{
				ManifestUrl: args[0],
				Force:       force,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.CloudInitImportRepoProgress_Status:
					fmt.Fprintln(out, e.Status)
				case *anvilv1.CloudInitImportRepoProgress_Error:
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.CloudInitImportRepoProgress_Imported:
					r := e.Imported
					switch {
					case r.GetError() != "":
						fmt.Fprintf(out, "%s: FAILED: %s\n", r.GetName(), r.GetError())
					case r.GetSkipped():
						fmt.Fprintf(out, "%s: skipped (already exists, use --force to overwrite)\n", r.GetName())
					default:
						fmt.Fprintf(out, "%s: imported\n", r.GetName())
					}
				}
			}
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a saved config that already has this name")
	return cmd
}

// editInEditor writes initial to a temp file, opens it in $EDITOR (falling back
// to vi), and returns the edited content once the editor exits.
func editInEditor(initial string) (string, error) {
	tmp, err := os.CreateTemp("", "anvil-cloud-init-*.yaml")
	if err != nil {
		return "", fmt.Errorf("cloud-init: creating temp file: %w", err)
	}
	path := tmp.Name()
	defer os.Remove(path)

	if _, err := tmp.WriteString(initial); err != nil {
		tmp.Close()
		return "", fmt.Errorf("cloud-init: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	editCmd := exec.Command(editor, path)
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr
	if err := editCmd.Run(); err != nil {
		return "", fmt.Errorf("cloud-init: running %s: %w", editor, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cloud-init: reading edited file: %w", err)
	}
	return string(data), nil
}
