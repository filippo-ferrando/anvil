package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func tasksTestModel() model {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, State: anvilv1.State_STATE_RUNNING})
	m.width, m.height = 120, 40
	m.tasks.setSize(90, 30)
	return m
}

func TestTrackedActionEndsWithItsReply(t *testing.T) {
	m := tasksTestModel()
	cmd := m.track("starting web", screenInstances, func() tea.Msg {
		return actionDoneMsg{screen: screenInstances, verb: "started"}
	})
	task := m.tasks.items[0]
	if !task.running || len(m.tasks.running()) != 1 {
		t.Fatal("expected a running task until the reply comes")
	}
	if strip := m.tasks.stripView(200); !strings.Contains(strip, "starting web") {
		t.Errorf("the strip should show the running task, got %q", strip)
	}
	if !strings.Contains(m.sidebarView(), "Tasks ⟳1") {
		t.Error("the sidebar should count the running tasks")
	}

	msg := cmd().(taskDoneMsg)
	next, _ := m.Update(msg)
	m = next.(model)
	if task.running || task.err != nil {
		t.Errorf("task running %v, err %v after its reply", task.running, task.err)
	}
	if m.status != "instance started" {
		t.Errorf("the reply should still set the status as before, got %q", m.status)
	}
	if m.tasks.stripView(200) != "" {
		t.Error("nothing runs any more, the strip should be gone")
	}
}

func TestTrackedFailureKeepsTheError(t *testing.T) {
	m := tasksTestModel()
	cmd := m.track("deleting web", screenInstances, func() tea.Msg {
		return actionDoneMsg{screen: screenInstances, verb: "deleted", err: errors.New("busy")}
	})
	next, _ := m.Update(cmd())
	m = next.(model)
	if task := m.tasks.items[0]; task.err == nil || !strings.Contains(taskRow(task), "busy") {
		t.Errorf("expected the failure in the task row, got %q", taskRow(task))
	}
}

func TestLaunchRunsInTheBackground(t *testing.T) {
	m := tasksTestModel()
	m.screen = screenLaunch
	m.launch.kind = "container"
	m.launch.form.fields = launchFields("container", nil)
	setField(&m.launch.form, "Image", "nginx:alpine")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if m.screen != screenInstances || cmd == nil {
		t.Fatalf("submitting should go back to Instances at once, on screen %v", m.screen)
	}
	if len(m.tasks.items) != 1 || m.tasks.items[0].title != "launching nginx:alpine" {
		t.Fatalf("expected a launch task, got %+v", m.tasks.items)
	}
	task := m.tasks.items[0]

	// The TUI stays usable: the delete confirmation still opens.
	next, _ = m.Update(key("d"))
	if len(next.(model).instances.confirmDelete) != 1 {
		t.Error("keys on Instances should work while the launch runs")
	}

	// The name the daemon picked starts following that instance's console.
	next, cmd = m.Update(launchTaskStep(task.id, "launched")(instanceStep{name: "nginx-1", next: func() tea.Msg { return nil }}))
	m = next.(model)
	if task.instance != "nginx-1" || task.logGen != 1 || cmd == nil {
		t.Fatalf("instance %q, log gen %d", task.instance, task.logGen)
	}
	next, _ = m.Update(taskLogMsg{id: task.id, gen: task.logGen, streamStep: streamStep[[]byte]{result: []byte("Booting Linux\ncloud-init: running modules\n")}})
	m = next.(model)
	if len(task.console.lines) != 2 {
		t.Errorf("console lines %q", task.console.lines)
	}
	next, _ = m.Update(taskLogMsg{id: task.id, gen: task.logGen - 1, streamStep: streamStep[[]byte]{result: []byte("stale\n")}})
	m = next.(model)
	if len(task.console.lines) != 2 {
		t.Error("output of a replaced log stream should be dropped")
	}

	m.tasks.open = task.id
	m.screen = screenTasks
	task.console.render()
	view := m.tasks.View()
	if !strings.Contains(view, "Console of nginx-1") || !strings.Contains(view, "cloud-init: running modules") {
		t.Errorf("the task view should show the console, got:\n%s", view)
	}

	done := launchTaskStep(task.id, "launched")(instanceStep{result: &anvilv1.Instance{Name: "nginx-1"}, done: true})
	next, _ = m.Update(done)
	m = next.(model)
	if task.running || task.logCancel != nil || m.status != "launched nginx:alpine" {
		t.Errorf("running %v, log open %v, status %q", task.running, task.logCancel != nil, m.status)
	}
	if !strings.Contains(task.last(), "launched nginx-1") {
		t.Errorf("last line %q", task.last())
	}
}

func TestTaskLogRetriesUntilTheInstanceExists(t *testing.T) {
	m := tasksTestModel()
	task, _ := m.streamTask("launching db", "launched db", screenInstances, func(context.Context, int) tea.Cmd { return nil })
	task.instance = "db"
	_ = m.followTaskLog(task)
	gen := task.logGen

	next, cmd := m.Update(taskLogMsg{id: task.id, gen: gen, streamStep: failed[[]byte](errors.New("not found"))})
	m = next.(model)
	if cmd == nil {
		t.Fatal("a missing log should be asked for again")
	}
	next, cmd = m.Update(taskLogRetryMsg{id: task.id, gen: gen})
	m = next.(model)
	if cmd == nil || task.logGen != gen+1 {
		t.Error("the retry should open the log again")
	}
	next, cmd = m.Update(taskLogRetryMsg{id: task.id, gen: gen})
	if cmd != nil {
		t.Error("a retry of a replaced stream should do nothing")
	}
	_ = next
}

func TestQuitAsksWhileTasksRun(t *testing.T) {
	m := tasksTestModel()
	m.sidebarFocused = true
	_ = m.track("deleting web", screenInstances, func() tea.Msg { return nil })

	next, cmd := m.Update(key("q"))
	m = next.(model)
	if cmd != nil || !m.statusBad || !strings.Contains(m.status, "still running") {
		t.Fatalf("the first q should warn, got status %q", m.status)
	}
	_, cmd = m.Update(key("q"))
	if cmd == nil {
		t.Fatal("the second q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("expected tea.Quit")
	}
}

func TestTasksScreenCancel(t *testing.T) {
	m := tasksTestModel()
	m.screen, m.sidebarFocused = screenTasks, false
	cancelled := false
	task, _ := m.streamTask("launching db", "launched db", screenInstances, func(context.Context, int) tea.Cmd { return nil })
	task.cancel = func() { cancelled = true }

	next, _ := m.Update(key("enter"))
	m = next.(model)
	if m.tasks.open != task.id {
		t.Fatal("enter should open the task")
	}
	next, _ = m.Update(key("x"))
	m = next.(model)
	if !strings.Contains(m.tasks.View(), "Cancel launching db?") {
		t.Fatal("x should ask before cancelling")
	}
	next, _ = m.Update(key("n"))
	m = next.(model)
	if cancelled {
		t.Fatal("any other key keeps it running")
	}
	next, _ = m.Update(key("x"))
	m = next.(model)
	next, _ = m.Update(key("y"))
	m = next.(model)
	if !cancelled {
		t.Error("y should cancel the task")
	}
	next, _ = m.Update(key("esc"))
	if next.(model).tasks.open != 0 {
		t.Error("esc should go back to the list")
	}
}

func TestFinishedTasksAreCapped(t *testing.T) {
	m := tasksTestModel()
	running, _ := m.streamTask("launching keep", "", screenInstances, func(context.Context, int) tea.Cmd { return nil })
	for i := range finishedTasksKept + 10 {
		cmd := m.track(fmt.Sprintf("action %d", i), screenInstances, func() tea.Msg { return nil })
		next, _ := m.Update(cmd())
		m = next.(model)
	}
	if got := len(m.tasks.items); got != finishedTasksKept+1 {
		t.Errorf("%d tasks kept, want %d", got, finishedTasksKept+1)
	}
	if m.tasks.items[0] != running {
		t.Error("a running task is never dropped")
	}
}

func TestDetailPanelShowsTheRunningTask(t *testing.T) {
	m := tasksTestModel()
	m.instances.setSize(100, 30)
	cmd := m.deleteTask([]string{"web"}, false)
	task := m.tasks.items[0]
	if cmd == nil || task.instance != "web" || task.logTail != deleteLogTail {
		t.Fatalf("a single delete should follow the console, got %+v", task)
	}
	task.console.appendLines("reboot: Power down\n")
	if view := m.instances.viewWith(m.tasks); !strings.Contains(view, "deleting web") || !strings.Contains(view, "Power down") {
		t.Errorf("the detail panel should show the task, got:\n%s", view)
	}
}

func TestConfirmedApplyRunsAsTask(t *testing.T) {
	m := tasksTestModel()
	a := &m.manifests.apply
	a.owner = screenManifests
	a.start(m.client, "manifest web", &anvilv1.IntentApplyRequest{})
	a.handle(applyStreamMsg{owner: screenManifests, streamStep: streamStep[applyPlanResult]{result: applyPlanResult{lines: []string{"plan"}}, done: true}, gen: a.gen})
	if len(m.tasks.items) != 0 {
		t.Fatal("a dry run should not start a task")
	}
	if cmd := m.applyKey(a, key("enter")); cmd == nil {
		t.Fatal("enter should send the real apply")
	}
	run := m.tasks.running()
	if len(run) != 1 || run[0].title != "applying manifest web" || run[0].cancel == nil {
		t.Fatalf("tasks %+v", run)
	}
	m.applyEvent(a, applyStreamMsg{owner: screenManifests, streamStep: streamStep[applyPlanResult]{status: "creating db"}, gen: a.gen})
	_, applied := m.applyEvent(a, applyStreamMsg{owner: screenManifests, streamStep: streamStep[applyPlanResult]{done: true}, gen: a.gen})
	if !applied || run[0].running || run[0].err != nil || run[0].last() != "creating db" {
		t.Fatalf("applied %v, task %+v", applied, run[0])
	}
}

func TestRunViewTaskReportsFailedClones(t *testing.T) {
	m := tasksTestModel()
	_, cancel := context.WithCancel(context.Background())
	m.ephemeral.run = newRunView([]string{"debian-13", "fedora-42"}, cancel)
	m.runTask(m.ephemeral.run, "running on debian-13, fedora-42", "ran", screenEphemeral)
	exit := func(d string, code int32) tea.Msg {
		return runEventMsg{ev: &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Exit{Exit: &anvilv1.RunExit{Distro: d, ExitCode: code}}}}
	}
	for _, msg := range []tea.Msg{
		runEventMsg{ev: &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Progress{Progress: &anvilv1.RunProgress{Distro: "debian-13", Message: "restoring"}}}},
		exit("debian-13", 0), exit("fedora-42", 2), runEventMsg{done: true},
	} {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	task := m.tasks.items[0]
	if task.running || task.err == nil || task.err.Error() != "1 of 2 clones failed" || !strings.Contains(task.steps.lines[0], "debian-13: restoring") {
		t.Fatalf("task %+v, lines %q", task, task.steps.lines)
	}
}

func TestCloudInitImportRunsAsTask(t *testing.T) {
	m := tasksTestModel()
	m.cloudInit.prompt = cloudInitPromptImportRepo
	m.cloudInit.promptFm = newSimpleForm("Import", []formField{textField("Manifest URL", "", "https://example.com/m.yaml"), toggleField("Force", "", false)})
	next, cmd := m.updateCloudInitPrompt(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	if cmd == nil || len(m.tasks.running()) != 1 {
		t.Fatalf("expected a running import task, got %d", len(m.tasks.running()))
	}
	id := m.cloudInit.importTask
	next, _ = m.Update(cloudInitImportStreamMsg{streamStep[*anvilv1.CloudInitImportResult]{result: &anvilv1.CloudInitImportResult{Name: "base"}}, id})
	next, _ = next.(model).Update(cloudInitImportStreamMsg{streamStep[*anvilv1.CloudInitImportResult]{done: true}, id})
	task := next.(model).tasks.items[0]
	if task.running || task.err != nil || !strings.Contains(task.last(), "base: imported") {
		t.Fatalf("task %+v", task)
	}
}

func TestDryRunApplyCancelsOnEsc(t *testing.T) {
	m := tasksTestModel()
	a := &m.manifests.apply
	a.owner = screenManifests
	a.start(m.client, "manifest web", &anvilv1.IntentApplyRequest{})
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	gen := a.gen
	m.applyKey(a, key("esc"))
	if ctx.Err() == nil || a.running || a.active() {
		t.Fatalf("esc should cancel the dry run: running %v, active %v", a.running, a.active())
	}
	a.handle(applyStreamMsg{owner: screenManifests, streamStep: failed[applyPlanResult](context.Canceled), gen: gen})
	if a.active() {
		t.Fatalf("the cancelled call's error came back: %q", a.lines)
	}
}
