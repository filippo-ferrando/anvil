package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/vars"
	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newApplyCommand(flags *globalFlags) *cobra.Command {
	var (
		file        string
		dryRun      bool
		prune       bool
		recreate    bool
		waitTimeout time.Duration
		varFlags    []string
		envFile     string
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
			overrides := map[string]string{}
			for _, kv := range varFlags {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("--var %q: expected KEY=VALUE", kv)
				}
				overrides[k] = v
			}
			values, err := vars.Load(file, envFile, data, overrides)
			if err != nil {
				return err
			}
			anvilPub, err := sshkey.EnsureDefaultPublic()
			if err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Intent.Apply(cmd.Context(), &anvilv1.IntentApplyRequest{
					Manifest:           data,
					DryRun:             dryRun,
					Prune:              prune,
					Recreate:           recreate,
					SshPublicKeys:      []string{anvilPub},
					WaitTimeoutSeconds: int32(waitTimeout.Seconds()),
					Vars:               values,
				})
				if err != nil {
					return err
				}
				return streamApply(cmd.OutOrStdout(), stream)
			})
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "anvil.yaml", `manifest to apply, or "-" for stdin`)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would change")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete members that are no longer in the manifest")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "allow replacing a VM whose image, cloud-init or SSH keys changed (its disk is lost)")
	cmd.Flags().StringArrayVar(&varFlags, "var", nil, "set a manifest variable as KEY=VALUE, over the shell env, .env and vars:")
	cmd.Flags().StringVar(&envFile, "env-file", "", "file of KEY=VALUE manifest variables (default: .env beside the manifest, if present)")
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
	return streamStatus(out, stream.Recv, func(l *liveLine, ev *anvilv1.IntentApplyProgress) (bool, error) {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentApplyProgress_Plan:
			l.finish()
			printApplyPlan(out, e.Plan)
		case *anvilv1.IntentApplyProgress_Status:
			l.set(e.Status)
		case *anvilv1.IntentApplyProgress_Error:
			return false, errors.New(e.Error)
		case *anvilv1.IntentApplyProgress_Intent:
			l.printf("Applied intent %q (%d members)\n", e.Intent.GetName(), len(e.Intent.GetMembers()))
		}
		return false, nil
	})
}

func printApplyPlan(out io.Writer, plan *anvilv1.IntentApplyPlan) {
	fmt.Fprintf(out, "Plan for intent %q:\n", plan.GetIntentName())
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range plan.GetSteps() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.GetAction(), s.GetRole(), s.GetInstanceName(), strings.Join(s.GetChanges(), "; "))
	}
	_ = tw.Flush()
}
