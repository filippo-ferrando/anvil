package commands

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"git.fferrando.cc/rdfilippo/anvil/internal/yamlcheck"
)

// newValidateCommand checks a saved document (fetched by get) or a local file with check.
// Warnings are printed but only errors fail the command.
func newValidateCommand(flags *globalFlags, what string, get func(ctx context.Context, flags *globalFlags, name string) (string, error),
	check func(content string) []yamlcheck.Problem) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate [<name>] [-f <file>]",
		Short: "Check a saved " + what + ", or a local file, for errors",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (file == "") == (len(args) == 0) {
				return fmt.Errorf("give either a saved %s name or -f <file>", what)
			}
			var content, label string
			if file != "" {
				data, err := readManifest(file)
				if err != nil {
					return err
				}
				content, label = string(data), file
			} else {
				var err error
				if content, err = get(cmd.Context(), flags, args[0]); err != nil {
					return err
				}
				label = args[0]
			}
			if n := printProblems(cmd.OutOrStdout(), label, check(content)); n > 0 {
				return fmt.Errorf("%s: %d error(s)", label, n)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", `local file to check, or "-" for stdin`)
	return cmd
}

// printProblems prints one "label:line: message" per problem and returns how many are errors.
func printProblems(out io.Writer, label string, problems []yamlcheck.Problem) int {
	errs := 0
	for _, p := range problems {
		where := label
		if p.Line > 0 {
			where = fmt.Sprintf("%s:%d", label, p.Line)
		}
		kind := "error"
		if p.Warning {
			kind = "warning"
		} else {
			errs++
		}
		fmt.Fprintf(out, "%s: %s: %s\n", where, kind, p.Message)
	}
	return errs
}
