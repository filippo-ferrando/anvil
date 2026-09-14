package commands

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// newFindCommand lists the VM base images available to launch, optionally
// filtered by a search term.
func newFindCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "find [term]",
		Short: "List VM base images available to launch (optionally filtered)",
		Long: "List the VM base images `anvil launch --kind vm <id>` can resolve: the " +
			"built-in multi-distro catalog plus any enabled `anvil mirror add --kind vm` " +
			"mirror. An optional term filters by id, distro, or version (case-insensitive " +
			"substring match).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tDISTRO\tVERSION\tARCH\tMIN DISK\tDEFAULT USER")
			for _, e := range reply.GetEntries() {
				if term != "" && !matchesFindTerm(e, term) {
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d GiB\t%s\n",
					e.GetId(), e.GetDistro(), e.GetVersion(), e.GetArch(), e.GetMinDiskGib(), e.GetDefaultUser())
			}
			return tw.Flush()
		},
	}
}

func matchesFindTerm(e *anvilv1.CatalogEntry, term string) bool {
	return strings.Contains(strings.ToLower(e.GetId()), term) ||
		strings.Contains(strings.ToLower(e.GetDistro()), term) ||
		strings.Contains(strings.ToLower(e.GetVersion()), term) ||
		strings.Contains(strings.ToLower(e.GetName()), term)
}
