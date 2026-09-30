package commands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
)

func newApplyCommand(flags *globalFlags) *cobra.Command {
	var (
		file        string
		dryRun      bool
		prune       bool
		recreate    bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "apply -f <anvil.yaml>",
		Short: "Create or update an intent from a manifest file",
		Long: "Compare an intent manifest with the intent's current state and apply the difference: " +
			"missing members are created, changed ones updated or replaced, stopped ones started. " +
			"Members start in depends_on order, each waiting until the ones it depends on are ready. " +
			"See docs/apply.md for the file format.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readManifest(file)
			if err != nil {
				return err
			}
			anvilPub, err := sshkey.EnsureDefaultPublic()
			if err != nil {
				return err
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.Intent.Apply(cmd.Context(), &anvilv1.IntentApplyRequest{
				Manifest:           data,
				DryRun:             dryRun,
				Prune:              prune,
				Recreate:           recreate,
				SshPublicKeys:      []string{anvilPub},
				WaitTimeoutSeconds: int32(waitTimeout.Seconds()),
			})
			if err != nil {
				return err
			}
			return streamApply(cmd.OutOrStdout(), stream)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "anvil.yaml", `manifest to apply, or "-" for stdin`)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would change")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete members that are no longer in the manifest")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "allow replacing a VM whose image, cloud-init or SSH keys changed (its disk is lost)")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "how long to wait for each member others depend on to be ready")
	return cmd
}

func readManifest(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	return data, nil
}

type applyStream interface {
	Recv() (*anvilv1.IntentApplyProgress, error)
}

func streamApply(out io.Writer, stream applyStream) error {
	term := isTerminalWriter(out)
	lastKey, haveLine := "", false
	endLine := func() {
		if haveLine {
			fmt.Fprintln(out)
			haveLine = false
		}
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			endLine()
			return nil
		}
		if err != nil {
			endLine()
			return err
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentApplyProgress_Plan:
			printApplyPlan(out, e.Plan)
		case *anvilv1.IntentApplyProgress_Status:
			if !term {
				fmt.Fprintln(out, e.Status)
				continue
			}
			// Updates with the same prefix (e.g. download percentages) redraw one line.
			key := e.Status
			if i := strings.LastIndex(key, ": "); i != -1 {
				key = key[:i]
			}
			if haveLine && key == lastKey {
				fmt.Fprint(out, "\r\033[K")
			} else {
				endLine()
			}
			fmt.Fprint(out, e.Status)
			lastKey, haveLine = key, true
		case *anvilv1.IntentApplyProgress_Error:
			endLine()
			return fmt.Errorf("%s", e.Error)
		case *anvilv1.IntentApplyProgress_Intent:
			endLine()
			fmt.Fprintf(out, "Applied intent %q (%d members)\n", e.Intent.GetName(), len(e.Intent.GetMembers()))
		}
	}
}

func printApplyPlan(out io.Writer, plan *anvilv1.IntentApplyPlan) {
	fmt.Fprintf(out, "Plan for intent %q:\n", plan.GetIntentName())
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range plan.GetSteps() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.GetAction(), s.GetRole(), s.GetInstanceName(), strings.Join(s.GetChanges(), "; "))
	}
	_ = tw.Flush()
}
