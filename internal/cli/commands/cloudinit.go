package commands

import (
	"fmt"
	"os"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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
			if err == nil {
				current = existing.GetContent()
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

// editInEditor writes initial to a temp file, opens $EDITOR (falling back
// to vi) on it attached to the real terminal, and returns the edited
// content once the editor exits.
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
