package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// applyStreamMsg carries events off an IntentService.Apply stream; result holds a plan's lines.
type applyStreamMsg struct{ streamStep[[]string] }

// newApplyForm is the A form on the Intents screen, the same settings as `anvil apply`.
func newApplyForm() simpleForm {
	return newSimpleForm("Apply manifest", []formField{
		pathField("Manifest path", "e.g. ./anvil.yaml", "anvil.yaml"),
		textField("Vars (optional)", "KEY=VALUE,KEY2=VALUE2, over the shell env, .env and vars:", ""),
		pathField("Env file (optional)", "KEY=VALUE file, blank = .env beside the manifest", ""),
		textField("Wait timeout", "how long to wait for each member others depend on", "15m"),
		toggleField("Prune", "delete members no longer in the manifest", false),
		toggleField("Recreate VMs", "allow replacing a VM, its disk is lost", false),
	})
}

// newApplyRequest reads the manifest on the client side, since the daemon can't see the caller's files.
// The request is a dry run; it is sent again for real once the plan is confirmed.
func newApplyRequest(form simpleForm) (*anvilv1.IntentApplyRequest, error) {
	path := form.Value("Manifest path")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	timeout, err := parseDurationField(form.Value("Wait timeout"), "Wait timeout")
	if err != nil {
		return nil, err
	}
	return client.NewApplyRequest(path, data, client.ApplyOptions{
		DryRun:      true,
		Prune:       form.Bool("Prune"),
		Recreate:    form.Bool("Recreate VMs"),
		Vars:        splitCommaList(form.Value("Vars (optional)")),
		EnvFile:     form.Value("Env file (optional)"),
		WaitTimeout: timeout,
	})
}

func startApplyStream(c *client.Client, req *anvilv1.IntentApplyRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Intent.Apply(context.Background(), req)
		if err != nil {
			return applyStreamMsg{failed[[]string](err)}
		}
		return recvApply(stream)()
	}
}

func recvApply(stream anvilv1.IntentService_ApplyClient) tea.Cmd {
	type step = streamStep[[]string]
	return recvStream(stream.Recv, func(ev *anvilv1.IntentApplyProgress) step {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentApplyProgress_Plan:
			return step{result: applyPlanLines(e.Plan)}
		case *anvilv1.IntentApplyProgress_Status:
			return step{status: e.Status}
		case *anvilv1.IntentApplyProgress_Error:
			return failed[[]string](errors.New(e.Error))
		case *anvilv1.IntentApplyProgress_Intent:
			return step{status: styleGood.Render("applied intent " + e.Intent.GetName())}
		}
		return step{}
	}, func(s step) tea.Msg { return applyStreamMsg{s} })
}

func applyPlanLines(plan *anvilv1.IntentApplyPlan) []string {
	lines := []string{fmt.Sprintf("Plan for intent %q:", plan.GetIntentName())}
	for _, s := range plan.GetSteps() {
		line := fmt.Sprintf("  %-8s %s (%s)", s.GetAction(), s.GetRole(), s.GetInstanceName())
		if len(s.GetChanges()) > 0 {
			line += "  " + strings.Join(s.GetChanges(), "; ")
		}
		lines = append(lines, line)
	}
	return append(lines, "")
}
