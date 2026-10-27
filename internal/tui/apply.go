package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/vars"
	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// applyStreamMsg carries events off an IntentService.Apply stream; result holds a plan's lines.
type applyStreamMsg struct{ streamStep[[]string] }

// newApplyRequest reads the manifest on the client side, since the daemon can't see the caller's files.
func newApplyRequest(path string, prune, recreate bool) (*anvilv1.IntentApplyRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	values, err := vars.Load(path, "", data, nil)
	if err != nil {
		return nil, err
	}
	pub, err := sshkey.EnsureDefaultPublic()
	if err != nil {
		return nil, err
	}
	return &anvilv1.IntentApplyRequest{Manifest: data, Prune: prune, Recreate: recreate, SshPublicKeys: []string{pub}, Vars: values}, nil
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
