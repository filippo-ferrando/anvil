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

// applyStreamMsg carries events off an IntentService.Apply stream; result holds a plan.
type applyStreamMsg struct {
	owner screen // the page that started the apply and shows its transcript
	streamStep[applyPlanResult]
	gen int // the applyRun.gen of the call, so a cancelled call's events are dropped
}

// applyPlanResult is a plan as shown, with the hash a confirmed apply sends back.
type applyPlanResult struct {
	lines []string
	hash  string
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

func startApplyStream(ctx context.Context, c *client.Client, req *anvilv1.IntentApplyRequest, owner screen, gen int) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Intent.Apply(ctx, req)
		if err != nil {
			return applyStreamMsg{owner, failed[applyPlanResult](err), gen}
		}
		return recvApply(stream, owner, gen)()
	}
}

func recvApply(stream anvilv1.IntentService_ApplyClient, owner screen, gen int) tea.Cmd {
	type step = streamStep[applyPlanResult]
	return recvStream(stream.Recv, func(ev *anvilv1.IntentApplyProgress) step {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentApplyProgress_Plan:
			return step{result: applyPlanResult{applyPlanLines(e.Plan), e.Plan.GetHash()}}
		case *anvilv1.IntentApplyProgress_Status:
			return step{status: e.Status}
		case *anvilv1.IntentApplyProgress_Error:
			return failed[applyPlanResult](errors.New(e.Error))
		case *anvilv1.IntentApplyProgress_Intent:
			return step{status: styleGood.Render("applied intent " + e.Intent.GetName())}
		}
		return step{}
	}, func(s step) tea.Msg { return applyStreamMsg{owner, s, gen} })
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
	what    string                      // the manifest it applies, for the task title
	req     *anvilv1.IntentApplyRequest // the request whose plan is shown, sent again for real on confirm
	task    int                         // the task of the real run, 0 before it starts
	cancel  context.CancelFunc
	running bool
	confirm bool // a dry run finished cleanly and waits for enter to apply
	lines   []string
	gen     int // bumped per Apply call, see applyStreamMsg
}

func (a *applyRun) active() bool { return a.running || len(a.lines) > 0 }

// start sends req as a dry run first, so the plan is shown before anything changes.
func (a *applyRun) start(c *client.Client, what string, req *anvilv1.IntentApplyRequest) tea.Cmd {
	req.DryRun, req.ExpectedPlanHash = true, ""
	a.what, a.req, a.running, a.confirm, a.lines, a.task = what, req, true, false, nil, 0
	a.gen++
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	return startApplyStream(ctx, c, req, a.owner, a.gen)
}

// handle records one stream event; applied is true once a real (not dry) run has ended.
func (a *applyRun) handle(msg applyStreamMsg) (cmd tea.Cmd, applied bool) {
	if msg.gen != a.gen {
		return nil, false // a cancelled call
	}
	a.lines = append(a.lines, msg.result.lines...)
	if msg.result.hash != "" && a.req.GetDryRun() {
		a.req.ExpectedPlanHash = msg.result.hash // the real run fails if its plan differs
	}
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
// esc also cancels a running dry run; a real run is cancelled from its task.
func (a *applyRun) key(msg tea.KeyMsg, c *client.Client) tea.Cmd {
	if a.running {
		switch msg.String() {
		case "esc", "q":
			if a.req.GetDryRun() {
				a.cancel()
				a.gen++
				a.running, a.lines, a.confirm = false, nil, false
			}
		}
		return nil
	}
	switch msg.String() {
	case "enter", "y":
		if a.confirm {
			a.req.DryRun = false
			a.confirm, a.running, a.lines = false, true, nil
			ctx, cancel := context.WithCancel(context.Background())
			a.cancel = cancel
			a.gen++
			return startApplyStream(ctx, c, a.req, a.owner, a.gen)
		}
		a.lines = nil
	case "esc", "q", "n":
		a.lines, a.confirm = nil, false
	}
	return nil
}

// applyKey handles a key on a's transcript and runs a confirmed plan as a task.
func (m *model) applyKey(a *applyRun, msg tea.KeyMsg) tea.Cmd {
	cmd := a.key(msg, m.client)
	if a.running && !a.req.GetDryRun() && a.task == 0 {
		t := m.startTask("applying "+a.what, "applied "+a.what, a.owner)
		t.steps.start()
		t.cancel = a.cancel
		t.cancelNote = "Stops the apply where it is. Members already changed stay as they are."
		a.task = t.id
	}
	return cmd
}

// applyEvent records one stream event on a and on the task of a real run.
func (m *model) applyEvent(a *applyRun, msg applyStreamMsg) (cmd tea.Cmd, applied bool) {
	if msg.gen != a.gen {
		return nil, false
	}
	cmd, applied = a.handle(msg)
	t := m.tasks.get(a.task)
	if t == nil || !t.running || a.req.GetDryRun() {
		return cmd, applied
	}
	if msg.status != "" {
		t.steps.add(msg.status)
	}
	if msg.err != nil || msg.done {
		return tea.Batch(cmd, m.finishTask(t, msg.err)), applied
	}
	return cmd, applied
}

func (a applyRun) view() string {
	s := styleTitle.Render(" Apply ") + "\n\n" + strings.Join(a.lines, "\n") + "\n\n"
	switch {
	case a.confirm:
		s += helpBar("enter", "apply", "esc", "cancel")
	case a.running && a.req.GetDryRun():
		s += helpBar("esc", "cancel")
	case !a.running:
		s += helpBar("esc", "dismiss")
	}
	return s
}
