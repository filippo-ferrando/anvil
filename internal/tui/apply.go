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
type applyStreamMsg struct {
	owner screen // the page that started the apply and shows its transcript
	streamStep[[]string]
}

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

func startApplyStream(c *client.Client, req *anvilv1.IntentApplyRequest, owner screen) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Intent.Apply(context.Background(), req)
		if err != nil {
			return applyStreamMsg{owner, failed[[]string](err)}
		}
		return recvApply(stream, owner)()
	}
}

func recvApply(stream anvilv1.IntentService_ApplyClient, owner screen) tea.Cmd {
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
	}, func(s step) tea.Msg { return applyStreamMsg{owner, s} })
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

// applyRun is an apply's on-screen state: a dry run's plan waiting for confirmation, then the real run.
type applyRun struct {
	owner   screen
	req     *anvilv1.IntentApplyRequest // the request whose plan is shown, sent again for real on confirm
	running bool
	confirm bool // a dry run finished cleanly and waits for enter to apply
	lines   []string
}

func (a *applyRun) active() bool { return a.running || len(a.lines) > 0 }

// start sends req as a dry run first, so the plan is shown before anything changes.
func (a *applyRun) start(c *client.Client, req *anvilv1.IntentApplyRequest) tea.Cmd {
	req.DryRun = true
	a.req, a.running, a.confirm, a.lines = req, true, false, nil
	return startApplyStream(c, req, a.owner)
}

// handle records one stream event; applied is true once a real (not dry) run has ended.
func (a *applyRun) handle(msg applyStreamMsg) (cmd tea.Cmd, applied bool) {
	a.lines = append(a.lines, msg.result...)
	if msg.status != "" {
		a.lines = appendProgressLine(a.lines, msg.status)
	}
	if msg.err != nil {
		a.running = false
		a.lines = append(a.lines, errLine(msg.err))
		return nil, false
	}
	if !msg.done {
		return msg.next, false
	}
	a.running = false
	if a.req.GetDryRun() {
		a.confirm = true
		return nil, false
	}
	return nil, true
}

// key handles a key press while the transcript is up: enter applies a confirmed plan, esc dismisses.
func (a *applyRun) key(msg tea.KeyMsg, c *client.Client) tea.Cmd {
	if a.running {
		return nil
	}
	switch msg.String() {
	case "enter", "y":
		if a.confirm {
			a.req.DryRun = false
			a.confirm, a.running, a.lines = false, true, nil
			return startApplyStream(c, a.req, a.owner)
		}
		a.lines = nil
	case "esc", "q", "n":
		a.lines, a.confirm = nil, false
	}
	return nil
}

func (a applyRun) view() string {
	s := styleTitle.Render(" Apply ") + "\n\n" + strings.Join(a.lines, "\n") + "\n\n"
	switch {
	case a.confirm:
		s += helpBar("enter", "apply", "esc", "cancel")
	case !a.running:
		s += helpBar("esc", "dismiss")
	}
	return s
}
