package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
	"github.com/anvil-project/anvil/pkg/client"
)

// applyStreamMsg carries events off an IntentService.Apply stream.
type applyStreamMsg struct {
	stream anvilv1.IntentService_ApplyClient
	lines  []string
	err    error
	done   bool
}

// newApplyRequest reads the manifest on the client side, since the daemon can't see the caller's files.
func newApplyRequest(path string, prune, recreate bool) (*anvilv1.IntentApplyRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	pub, err := sshkey.EnsureDefaultPublic()
	if err != nil {
		return nil, err
	}
	return &anvilv1.IntentApplyRequest{Manifest: data, Prune: prune, Recreate: recreate, SshPublicKeys: []string{pub}}, nil
}

func startApplyStream(c *client.Client, req *anvilv1.IntentApplyRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Intent.Apply(context.Background(), req)
		if err != nil {
			return applyStreamMsg{err: err, done: true}
		}
		return receiveApplyEvent(stream)()
	}
}

func receiveApplyEvent(stream anvilv1.IntentService_ApplyClient) tea.Cmd {
	return func() tea.Msg {
		ev, err := stream.Recv()
		if err == io.EOF {
			return applyStreamMsg{done: true}
		}
		if err != nil {
			return applyStreamMsg{err: err, done: true}
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentApplyProgress_Plan:
			return applyStreamMsg{stream: stream, lines: applyPlanLines(e.Plan)}
		case *anvilv1.IntentApplyProgress_Status:
			return applyStreamMsg{stream: stream, lines: []string{e.Status}}
		case *anvilv1.IntentApplyProgress_Error:
			return applyStreamMsg{err: fmt.Errorf("%s", e.Error), done: true}
		case *anvilv1.IntentApplyProgress_Intent:
			return applyStreamMsg{stream: stream, lines: []string{styleGood.Render("applied intent " + e.Intent.GetName())}}
		default:
			return applyStreamMsg{stream: stream}
		}
	}
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
