package commands

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// newFindCommand lists the VM base images available to launch, optionally
// filtered by a search term.
func newFindCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "find [term]",
		Short: "List VM base images available to launch (optionally filtered)",
		Long: "List the VM base images `anvil launch --kind vm <id>` can resolve: the " +
			"built-in multi-distro catalog plus any enabled `anvil mirror add --kind vm` " +
			"mirror. An optional term filters by id, distro, or version (case-insensitive " +
			"substring match).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Image.Catalog(cmd.Context(), &anvilv1.CatalogRequest{})
			if err != nil {
				return err
			}

			var term string
			if len(args) == 1 {
				term = strings.ToLower(args[0])
			}

			var entries []*anvilv1.CatalogEntry
			for _, e := range reply.GetEntries() {
				if term == "" || matchesFindTerm(e, term) {
					entries = append(entries, e)
				}
			}
			if output == "json" {
				return printJSON(cmd.OutOrStdout(), &anvilv1.CatalogReply{Entries: entries})
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tDISTRO\tVERSION\tARCH\tMIN DISK\tDEFAULT USER\tFIRECRACKER")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d GiB\t%s\t%s\n",
					e.GetId(), e.GetDistro(), e.GetVersion(), e.GetArch(), e.GetMinDiskGib(), e.GetDefaultUser(), yesNo(e.GetFirecracker()))
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func matchesFindTerm(e *anvilv1.CatalogEntry, term string) bool {
	return strings.Contains(strings.ToLower(e.GetId()), term) ||
		strings.Contains(strings.ToLower(e.GetDistro()), term) ||
		strings.Contains(strings.ToLower(e.GetVersion()), term) ||
		strings.Contains(strings.ToLower(e.GetName()), term)
}
