package commands

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// validateFunc sends content to the daemon to check; file is the local path it came from, "" for a saved one.
type validateFunc func(ctx context.Context, c *client.Client, file string, content []byte) ([]*anvilv1.ValidationProblem, error)

// newValidateCommand checks a saved document (fetched by get) or a local file on the daemon.
// Warnings are printed but only errors fail the command.
func newValidateCommand(flags *globalFlags, what string, get func(ctx context.Context, c *client.Client, name string) (string, error),
	check validateFunc) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate [<name>] [-f <file>]",
		Short: "Check a saved " + what + ", or a local file, for errors",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (file == "") == (len(args) == 0) {
				return fmt.Errorf("give either a saved %s name or -f <file>", what)
			}
			var content []byte
			label := file
			if file != "" {
				data, err := readManifest(file)
				if err != nil {
					return err
				}
				content = data
			}
			return withClient(flags, func(c *client.Client) error {
				if file == "" {
					label = args[0]
					saved, err := get(cmd.Context(), c, label)
					if err != nil {
						return err
					}
					content = []byte(saved)
				}
				problems, err := check(cmd.Context(), c, file, content)
				if err != nil {
					return err
				}
				if n := printProblems(cmd.OutOrStdout(), label, problems); n > 0 {
					return fmt.Errorf("%s: %d error(s)", label, n)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", `local file to check, or "-" for stdin`)
	return cmd
}

// printProblems prints one "label:line: message" per problem and returns how many are errors.
func printProblems(out io.Writer, label string, problems []*anvilv1.ValidationProblem) int {
	errs := 0
	for _, p := range problems {
		where := label
		if p.GetLine() > 0 {
			where = fmt.Sprintf("%s:%d", label, p.GetLine())
		}
		kind := "error"
		if p.GetWarning() {
			kind = "warning"
		} else {
			errs++
		}
		fmt.Fprintf(out, "%s: %s: %s\n", where, kind, p.GetMessage())
	}
	return errs
}
