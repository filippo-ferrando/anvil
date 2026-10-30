package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// The Ephemeral screen: warm snapshots and clones, a form to build one or start a run,
// and the run view with one pane per clone.

type warmItem struct{ snap *anvilv1.WarmSnapshot }

func (i warmItem) FilterValue() string { return i.snap.GetDistro() }
func (i warmItem) Title() string {
	name := i.snap.GetDistro()
	if i.snap.GetApp() != "" {
		name += " + " + i.snap.GetApp()
	}
	return name + "  " + i.snap.GetKey()
}
func (i warmItem) Description() string {
	return fmt.Sprintf("%s  •  %s  •  %d MiB  •  %s  •  last used %s", i.snap.GetArch(), format.CPUs(i.snap.GetCpus()),
		i.snap.GetMemoryMib(), progress.Bytes(i.snap.GetSizeBytes()), time.Unix(i.snap.GetLastUsedAtUnix(), 0).Format("2006-01-02 15:04"))
}

type cloneItem struct{ clone *anvilv1.EphemeralClone }

func (i cloneItem) FilterValue() string { return i.clone.GetId() }
func (i cloneItem) Title() string       { return i.clone.GetId() }
func (i cloneItem) Description() string {
	state := "running"
	switch {
	case i.clone.GetKept():
		state = fmt.Sprintf("kept, exited %d", i.clone.GetExitCode())
	case i.clone.GetExited():
		state = fmt.Sprintf("exited %d", i.clone.GetExitCode())
	}
	return fmt.Sprintf("%s  •  %s  •  up %s", i.clone.GetDistro(), state, time.Since(time.Unix(i.clone.GetStartedAtUnix(), 0)).Round(time.Second))
}

type ephemeralModel struct {
	showClones bool // the list on screen: false warm snapshots, true clones
	warm       list.Model
	clones     list.Model

	form     *simpleForm // the open form, nil for none
	formKind string      // "warm" | "run" | "app"
	confirm  string      // the warm key or clone id waiting for a yes
	building transcript  // a warm snapshot build in progress
	run      *runView
}

func newEphemeralModel() ephemeralModel {
	w, c := newList(), newList()
	w.Title, c.Title = "Warm snapshots", "Clones"
	for _, l := range []*list.Model{&w, &c} {
		l.SetFilteringEnabled(false)
		l.SetShowHelp(false)
	}
	return ephemeralModel{warm: w, clones: c}
}

func (e *ephemeralModel) setSize(w, h int) {
	e.warm.SetSize(w-2, h-1)
	e.clones.SetSize(w-2, h-1)
}

// runView is a run in progress or done: one pane per distro, in the order asked.
type runView struct {
	order  []string
	panes  map[string]*runPane
	cancel context.CancelFunc
	done   bool
	err    error
}

// runPane keeps the last lines of one clone's output.
type runPane struct {
	lines   []string
	partial string
	status  string
	since   time.Time // when status's step began
	exit    *anvilv1.RunExit
}

const paneKeep = 200

func newRunView(distros []string, cancel context.CancelFunc) *runView {
	v := &runView{order: distros, panes: map[string]*runPane{}, cancel: cancel}
	for _, d := range distros {
		v.panes[d] = &runPane{status: "waiting", since: time.Now()}
	}
	return v
}

func (p *runPane) write(data []byte) {
	text := p.partial + string(data)
	parts := strings.Split(text, "\n")
	p.partial = parts[len(parts)-1]
	p.lines = append(p.lines, parts[:len(parts)-1]...)
	if len(p.lines) > paneKeep {
		p.lines = p.lines[len(p.lines)-paneKeep:]
	}
}

// setStatus starts a new step, or updates the current one. A step that took a while stays
// in the pane, the same as in the CLI.
func (p *runPane) setStatus(status string) {
	if progress.Key(status) != progress.Key(p.status) {
		if elapsed := time.Since(p.since); elapsed >= time.Second && p.status != "waiting" {
			p.lines = append(p.lines, "· "+progress.Done(p.status, elapsed))
		}
		p.since = time.Now()
	}
	p.status = status
}

func (v *runView) event(ev *anvilv1.RunEvent) {
	switch {
	case ev.GetProgress() != nil:
		if p := v.panes[ev.GetProgress().GetDistro()]; p != nil {
			p.setStatus(ev.GetProgress().GetMessage())
		}
	case ev.GetOutput() != nil:
		if p := v.panes[ev.GetOutput().GetDistro()]; p != nil {
			p.write(ev.GetOutput().GetData())
		}
	case ev.GetExit() != nil:
		if p := v.panes[ev.GetExit().GetDistro()]; p != nil {
			if p.partial != "" {
				p.lines, p.partial = append(p.lines, p.partial), ""
			}
			p.exit = ev.GetExit()
		}
	}
}

func (v *runView) view(width, height int) string {
	var b strings.Builder
	title := "Run"
	if v.done {
		title = "Run finished"
	}
	b.WriteString(styleTitle.Render(" "+title+" ") + "\n")
	if v.err != nil {
		b.WriteString(styleError.Render(v.err.Error()) + "\n")
	}
	paneH := max(3, (height-3)/max(1, len(v.order)))
	for _, d := range v.order {
		p := v.panes[d]
		// The app's one-second tick moves the spinner on.
		head := styleSubtitle.Render(progress.Live(p.status, time.Since(p.since), int(time.Now().Unix())))
		if x := p.exit; x != nil {
			switch {
			case x.GetError() != "":
				head = styleError.Render(x.GetError())
			case x.GetExitCode() == 0:
				head = styleGood.Render(fmt.Sprintf("exit 0 in %s", time.Duration(x.GetDurationMs())*time.Millisecond))
			default:
				head = styleError.Render(fmt.Sprintf("exit %d", x.GetExitCode()))
				if x.GetKept() {
					head += styleWarn.Render("  kept as " + x.GetCloneId())
				}
			}
		}
		b.WriteString(styleKey.Render(d) + " " + head + "\n")
		lines := p.lines
		if p.partial != "" {
			lines = append(append([]string(nil), lines...), p.partial)
		}
		if n := paneH - 1; len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		for _, l := range lines {
			if len(l) > width-2 {
				l = l[:width-2]
			}
			b.WriteString("  " + l + "\n")
		}
	}
	if v.done {
		b.WriteString(helpBar("esc", "close"))
	} else {
		b.WriteString(helpBar("esc", "cancel (deletes the clones)"))
	}
	return b.String()
}

// Messages.

type warmLoadedMsg struct {
	snaps []*anvilv1.WarmSnapshot
	err   error
}

type clonesLoadedMsg struct {
	clones []*anvilv1.EphemeralClone
	err    error
}

type warmBuildMsg struct {
	step streamStep[*anvilv1.WarmSnapshot]
}

// runEventMsg carries one event of a TUI run, or its end.
type runEventMsg struct {
	ev   *anvilv1.RunEvent
	next tea.Cmd
	err  error
	done bool
}

func loadEphemeral(c *client.Client) tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			reply, err := c.Ephemeral.WarmList(context.Background(), &anvilv1.WarmListRequest{})
			return warmLoadedMsg{snaps: reply.GetSnapshots(), err: err}
		},
		func() tea.Msg {
			reply, err := c.Ephemeral.CloneList(context.Background(), &anvilv1.CloneListRequest{})
			return clonesLoadedMsg{clones: reply.GetClones(), err: err}
		},
	)
}

func buildWarm(c *client.Client, req *anvilv1.WarmCreateRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Ephemeral.WarmCreate(context.Background(), req)
		if err != nil {
			return warmBuildMsg{failed[*anvilv1.WarmSnapshot](err)}
		}
		parse := func(ev *anvilv1.WarmCreateProgress) streamStep[*anvilv1.WarmSnapshot] {
			return streamStep[*anvilv1.WarmSnapshot]{status: ev.GetStatus(), result: ev.GetSnapshot(), done: ev.GetSnapshot() != nil}
		}
		return recvStream(stream.Recv, parse, func(s streamStep[*anvilv1.WarmSnapshot]) tea.Msg { return warmBuildMsg{s} })()
	}
}

// startRun opens the Run stream, sends the request and the upload, then hands over the
// first event; each runEventMsg carries the command that receives the next one.
func startRun(ctx context.Context, c *client.Client, req *anvilv1.RunRequest, path string) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Ephemeral.Run(ctx)
		if err == nil {
			err = stream.Send(&anvilv1.RunInput{Input: &anvilv1.RunInput_Request{Request: req}})
		}
		if err == nil && path != "" {
			err = sendRunUpload(stream, path)
		}
		if err != nil {
			return runEventMsg{err: err, done: true}
		}
		var next tea.Cmd
		next = func() tea.Msg {
			ev, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return runEventMsg{done: true}
			}
			if err != nil {
				return runEventMsg{err: err, done: true}
			}
			return runEventMsg{ev: ev, next: next}
		}
		return next()
	}
}

func sendRunUpload(stream anvilv1.EphemeralService_RunClient, path string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(client.WriteUploadTar(path, pw)) }()
	buf := make([]byte, 256<<10)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if serr := stream.Send(&anvilv1.RunInput{Input: &anvilv1.RunInput_UploadChunk{UploadChunk: append([]byte(nil), buf[:n]...)}}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return stream.Send(&anvilv1.RunInput{Input: &anvilv1.RunInput_UploadDone{UploadDone: true}})
		}
		if err != nil {
			return err
		}
	}
}

func deleteWarm(c *client.Client, key string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Ephemeral.WarmDelete(context.Background(), &anvilv1.WarmDeleteRequest{Keys: []string{key}})
		return actionDoneMsg{screen: screenEphemeral, verb: "removed warm snapshot " + key, err: err}
	}
}

func deleteClone(c *client.Client, id string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Ephemeral.CloneDelete(context.Background(), &anvilv1.CloneDeleteRequest{Ids: []string{id}})
		return actionDoneMsg{screen: screenEphemeral, verb: "deleted clone " + id, err: err}
	}
}

// Forms.

func newWarmForm() simpleForm {
	return newSimpleForm("Build a warm snapshot", []formField{
		textField("Distro", "catalog id, e.g. ubuntu-24.04", ""),
		textField("CPUs", "default 2", ""),
		textField("Memory (MiB)", "default 512", ""),
	})
}

func newRunForm() simpleForm {
	return newSimpleForm("Run on throwaway clones", []formField{
		textField("Distros", "comma separated, e.g. ubuntu-24.04,debian-13", ""),
		textField("Path", "a directory to upload into /work (optional)", ""),
		textField("Command", "run with sh -c", ""),
		textField("CPUs", "default 2", ""),
		textField("Memory (MiB)", "default 512", ""),
		toggleField("Keep failed", "keep clones whose command failed", false),
	})
}

func newAppForm() simpleForm {
	return newSimpleForm("GUI app in a throwaway clone", []formField{
		textField("Distro", "catalog id, e.g. debian-13", ""),
		textField("Package", "installed with the distro's package manager, e.g. firefox-esr", ""),
		textField("Command (optional)", "default: the package's desktop entry", ""),
		toggleField("X11 app", "Java/Swing and other X11 apps need XWayland", false),
		textField("CPUs", "default 2", ""),
		textField("Memory (MiB)", "default 2048", ""),
		toggleField("Add to desktop", "also add it to this desktop's app launcher", false),
	})
}

func buildAppRequest(form simpleForm) (appRequest, error) {
	a := appRequest{
		distro: strings.TrimSpace(form.Value("Distro")), pkg: strings.TrimSpace(form.Value("Package")),
		command: form.Value("Command (optional)"), x11: form.Bool("X11 app"), export: form.Bool("Add to desktop"),
	}
	if a.distro == "" || a.pkg == "" {
		return a, errors.New("distro and package are required")
	}
	var err error
	if a.cpus, err = optionalInt(form, "CPUs"); err != nil {
		return a, err
	}
	a.memory, err = optionalInt(form, "Memory (MiB)")
	return a, err
}

func optionalInt(form simpleForm, label string) (int64, error) {
	s := strings.TrimSpace(form.Value(label))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a positive number", strings.ToLower(label))
	}
	return n, nil
}

func buildWarmRequest(form simpleForm) (*anvilv1.WarmCreateRequest, error) {
	distro := strings.TrimSpace(form.Value("Distro"))
	if distro == "" {
		return nil, errors.New("distro is required")
	}
	cpus, err := optionalInt(form, "CPUs")
	if err != nil {
		return nil, err
	}
	mem, err := optionalInt(form, "Memory (MiB)")
	if err != nil {
		return nil, err
	}
	return &anvilv1.WarmCreateRequest{Distro: distro, Cpus: int32(cpus), MemoryMib: mem}, nil
}

// buildRunRequest turns the run form into a request and the path to upload ("" for none).
func buildRunRequest(form simpleForm) (*anvilv1.RunRequest, string, error) {
	var distros []string
	for _, d := range strings.Split(form.Value("Distros"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			distros = append(distros, d)
		}
	}
	command := strings.TrimSpace(form.Value("Command"))
	if len(distros) == 0 || command == "" {
		return nil, "", errors.New("distros and a command are required")
	}
	path := strings.TrimSpace(form.Value("Path"))
	if path != "" {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return nil, "", fmt.Errorf("%s is not a directory", path)
		}
	}
	cpus, err := optionalInt(form, "CPUs")
	if err != nil {
		return nil, "", err
	}
	mem, err := optionalInt(form, "Memory (MiB)")
	if err != nil {
		return nil, "", err
	}
	return &anvilv1.RunRequest{
		Distros: distros, Command: []string{"sh", "-c", command}, Cpus: int32(cpus), MemoryMib: mem,
		KeepFailed: form.Bool("Keep failed"), Upload: path != "",
	}, path, nil
}

// Update.

func (m model) updateEphemeral(msg tea.Msg) (tea.Model, tea.Cmd) {
	e := &m.ephemeral
	switch msg := msg.(type) {
	case warmLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing warm snapshots: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.snaps))
		for i, s := range msg.snaps {
			items[i] = warmItem{snap: s}
		}
		e.warm.SetItems(items)
		return m, nil
	case clonesLoadedMsg:
		if msg.err == nil {
			items := make([]list.Item, len(msg.clones))
			for i, c := range msg.clones {
				items[i] = cloneItem{clone: c}
			}
			e.clones.SetItems(items)
		}
		return m, nil
	case warmBuildMsg:
		if msg.step.status != "" {
			e.building.add(msg.step.status)
		}
		if msg.step.err != nil {
			e.building.add("failed: " + msg.step.err.Error())
		}
		if msg.step.done {
			e.building.running = false
			return m, loadEphemeral(m.client)
		}
		return m, msg.step.next
	case runEventMsg:
		if e.run == nil {
			return m, nil
		}
		if msg.ev != nil {
			e.run.event(msg.ev)
		}
		if msg.done {
			e.run.done, e.run.err = true, msg.err
			return m, loadEphemeral(m.client)
		}
		return m, msg.next
	case guiLaunchedMsg:
		m.showGUILaunched(msg)
		return m, loadEphemeral(m.client)
	case actionDoneMsg:
		m.setStatus(msg.verb, msg.err != nil)
		if msg.err != nil {
			m.setStatus(msg.verb+": "+msg.err.Error(), true)
		}
		return m, loadEphemeral(m.client)
	case tea.KeyMsg:
		return m.updateEphemeralKey(msg)
	}
	return m, nil
}

func (m model) updateEphemeralKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	e := &m.ephemeral
	if e.run != nil {
		if msg.String() == "esc" || msg.String() == "q" {
			if !e.run.done {
				e.run.cancel()
				return m, nil
			}
			e.run = nil
			return m, loadEphemeral(m.client)
		}
		return m, nil
	}
	if e.building.active() {
		e.building.dismiss(msg.String())
		return m, nil
	}
	if e.confirm != "" {
		target := e.confirm
		e.confirm = ""
		if msg.String() == "y" || msg.String() == "enter" {
			if e.showClones {
				return m, deleteClone(m.client, target)
			}
			return m, deleteWarm(m.client, target)
		}
		return m, nil
	}
	if e.form != nil {
		form, submitted, cancelled := e.form.update(msg)
		e.form = &form
		if cancelled {
			e.form = nil
			return m, nil
		}
		if !submitted {
			return m, nil
		}
		if e.formKind == "app" {
			a, err := buildAppRequest(form)
			if err == nil && m.client.Remote != "" {
				err = errors.New("GUI apps in throwaway clones don't work with --remote yet")
			}
			if err != nil {
				e.form.errMsg = err.Error()
				return m, nil
			}
			e.form = nil
			if a.export {
				m.setStatus("building the warm snapshot for "+a.pkg+", then starting it", false)
			}
			return m, launchEphemeralApp(m.socketPath, a)
		}
		if e.formKind == "warm" {
			req, err := buildWarmRequest(form)
			if err != nil {
				e.form.errMsg = err.Error()
				return m, nil
			}
			e.form = nil
			e.building.start()
			return m, buildWarm(m.client, req)
		}
		req, path, err := buildRunRequest(form)
		if err != nil {
			e.form.errMsg = err.Error()
			return m, nil
		}
		e.form = nil
		ctx, cancel := context.WithCancel(context.Background())
		e.run = newRunView(req.GetDistros(), cancel)
		return m, startRun(ctx, m.client, req, path)
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "tab":
		e.showClones = !e.showClones
		return m, nil
	case "r":
		return m, loadEphemeral(m.client)
	case "c":
		f := newWarmForm()
		f.SetHeight(contentHeight(m.height) - 2)
		e.form, e.formKind = &f, "warm"
		return m, nil
	case "a":
		f := newAppForm()
		f.SetHeight(contentHeight(m.height) - 2)
		e.form, e.formKind = &f, "app"
		return m, nil
	case "n":
		f := newRunForm()
		f.SetHeight(contentHeight(m.height) - 2)
		e.form, e.formKind = &f, "run"
		return m, nil
	case "x":
		if e.showClones {
			if it, ok := e.clones.SelectedItem().(cloneItem); ok {
				e.confirm = it.clone.GetId()
			}
		} else if it, ok := e.warm.SelectedItem().(warmItem); ok {
			e.confirm = it.snap.GetKey()
		}
		return m, nil
	case "s", "enter":
		if it, ok := e.clones.SelectedItem().(cloneItem); ok && e.showClones {
			return m, m.cloneShell(it.clone.GetId())
		}
		return m, nil
	}
	var cmd tea.Cmd
	if e.showClones {
		e.clones, cmd = e.clones.Update(msg)
	} else {
		e.warm, cmd = e.warm.Update(msg)
	}
	return m, cmd
}

// cloneShell suspends the TUI for `anvil ephemeral shell <id>`.
func (m model) cloneShell(id string) tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return shellDoneMsg{err: err} }
	}
	args := []string{}
	if m.socketPath != "" {
		args = append(args, "--socket", m.socketPath)
	}
	if m.client.Remote != "" {
		args = append(args, "--remote", m.client.Remote)
	}
	cmd := exec.Command(exe, append(args, "ephemeral", "shell", id)...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

func (e ephemeralModel) View(width, height int) string {
	switch {
	case e.run != nil:
		return e.run.view(width, height)
	case e.building.active():
		return e.building.view("Building a warm snapshot")
	case e.confirm != "":
		what := "warm snapshot"
		if e.showClones {
			what = "clone"
		}
		return styleWarn.Render("Remove "+what+" "+e.confirm+"?") + "\n\n" + helpBar("y", "confirm", "any other key", "cancel")
	case e.form != nil:
		return e.form.View()
	}
	l, other := e.warm, "clones"
	if e.showClones {
		l, other = e.clones, "warm snapshots"
	}
	keys := []string{"tab", other, "n", "run", "a", "gui app", "c", "build warm", "x", "remove", "r", "refresh", "esc", "back"}
	if e.showClones {
		keys = append([]string{"s", "shell"}, keys...)
	}
	return l.View() + "\n" + helpBar(keys...)
}
