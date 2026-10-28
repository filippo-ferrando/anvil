package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
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
		yes         bool
		saved       string
	)
	cmd := &cobra.Command{
		Use:   "apply [-f <anvil.yaml> | --saved <name>]",
		Short: "Create or update an intent from a manifest file",
		Long: "Compare an intent manifest with the intent's current state and apply the difference: " +
			"missing members are created, changed ones updated or replaced, stopped ones started. " +
			"Members start in depends_on order, each waiting until the ones it depends on are ready. " +
			"On a terminal the plan is shown first and applied only after confirmation; -y skips the question. " +
			"See docs/apply.md for the file format.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := loadManifest(cmd.Context(), flags, file, saved)
			if err != nil {
				return err
			}
			if saved != "" {
				file = "" // no file: .env is read from the current dir
			}
			req, err := client.NewApplyRequest(file, data, client.ApplyOptions{
				DryRun: dryRun, Prune: prune, Recreate: recreate,
				Vars: varFlags, EnvFile: envFile, WaitTimeout: waitTimeout,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var ask func() bool
			if shouldConfirmApply(dryRun, yes, file, out) {
				ask = func() bool { return confirm(os.Stdin, out, "Apply these changes?") }
			}
			return withClient(flags, func(c *client.Client) error {
				start := func(dry bool) (applyStream, error) {
					r := proto.CloneOf(req)
					r.DryRun = r.DryRun || dry
					return c.Intent.Apply(cmd.Context(), r)
				}
				return runApply(out, start, ask)
			})
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "anvil.yaml", `manifest to apply, or "-" for stdin`)
	cmd.Flags().StringVar(&saved, "saved", "", "apply a manifest saved with 'anvil manifest' instead of a file")
	cmd.MarkFlagsMutuallyExclusive("file", "saved")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would change")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply without asking for confirmation on a terminal")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete members that are no longer in the manifest")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "allow replacing a VM whose image, cloud-init or SSH keys changed (its disk is lost)")
	cmd.Flags().StringArrayVar(&varFlags, "var", nil, "set a manifest variable as KEY=VALUE, over the shell env, .env and vars:")
	cmd.Flags().StringVar(&envFile, "env-file", "", "file of KEY=VALUE manifest variables (default: .env beside the manifest, if present)")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "how long to wait for each member others depend on to be ready")
	return cmd
}

// loadManifest reads the manifest from the saved library when saved is set, else from path.
func loadManifest(ctx context.Context, flags *globalFlags, path, saved string) ([]byte, error) {
	if saved == "" {
		return readManifest(path)
	}
	var data []byte
	err := withClient(flags, func(c *client.Client) error {
		reply, err := c.Manifest.Get(ctx, &anvilv1.ManifestGetRequest{Name: saved})
		data = []byte(reply.GetContent())
		return err
	})
	return data, err
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

// applyInteractive reports whether stdin and out are terminals; tests replace it.
var applyInteractive = func(out io.Writer) bool {
	return term.IsTerminal(os.Stdin.Fd()) && isTerminalWriter(out)
}

// shouldConfirmApply is true for a real apply on a terminal without --yes.
// A manifest read from stdin leaves no stdin to answer on.
func shouldConfirmApply(dryRun, yes bool, file string, out io.Writer) bool {
	return !dryRun && !yes && file != "-" && applyInteractive(out)
}

// runApply applies directly when ask is nil. Otherwise it shows a dry-run plan first
// and asks before applying, unless the plan changes nothing.
func runApply(out io.Writer, start func(dry bool) (applyStream, error), ask func() bool) error {
	if ask == nil {
		stream, err := start(false)
		if err != nil {
			return err
		}
		_, err = streamApply(out, stream, true)
		return err
	}

	stream, err := start(true)
	if err != nil {
		return err
	}
	plan, err := streamApply(out, stream, false)
	if err != nil {
		return err
	}
	if !planHasChanges(plan) {
		return runApply(out, start, nil)
	}
	printApplyPlan(out, plan)
	if !ask() {
		fmt.Fprintln(out, "Apply cancelled.")
		return nil
	}
	stream, err = start(false)
	if err != nil {
		return err
	}
	_, err = streamApply(out, stream, false)
	return err
}

func planHasChanges(plan *anvilv1.IntentApplyPlan) bool {
	for _, s := range plan.GetSteps() {
		if s.GetAction() != "keep" {
			return true
		}
	}
	return false
}

// confirm asks a y/N question on out and reads the answer from in; anything but y or yes is no.
func confirm(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N] ", question)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// streamApply prints the stream's progress, and its plan when showPlan is set, and returns the plan.
func streamApply(out io.Writer, stream applyStream, showPlan bool) (*anvilv1.IntentApplyPlan, error) {
	var plan *anvilv1.IntentApplyPlan
	err := streamStatus(out, stream.Recv, func(l *liveLine, ev *anvilv1.IntentApplyProgress) (bool, error) {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentApplyProgress_Plan:
			plan = e.Plan
			if showPlan {
				l.finish()
				printApplyPlan(out, e.Plan)
			}
		case *anvilv1.IntentApplyProgress_Status:
			l.set(e.Status)
		case *anvilv1.IntentApplyProgress_Error:
			return false, errors.New(e.Error)
		case *anvilv1.IntentApplyProgress_Intent:
			l.printf("Applied intent %q (%d members)\n", e.Intent.GetName(), len(e.Intent.GetMembers()))
		}
		return false, nil
	})
	return plan, err
}

func printApplyPlan(out io.Writer, plan *anvilv1.IntentApplyPlan) {
	fmt.Fprintf(out, "Plan for intent %q:\n", plan.GetIntentName())
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range plan.GetSteps() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.GetAction(), s.GetRole(), s.GetInstanceName(), strings.Join(s.GetChanges(), "; "))
	}
	_ = tw.Flush()
}
