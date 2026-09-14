package tui

import (
	"context"
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
	"github.com/anvil-project/anvil/pkg/client"
)

// cloudInitNameField is the VM-only field naming a saved library entry —
// shared between newLaunchModel/launchFields and buildLaunchRequest so
// the two can't drift out of sync.
const cloudInitNameField = "Cloud-init name (optional)"

type launchModel struct {
	kind          string // "vm" | "container"
	form          simpleForm
	launching     bool
	progressLines []string

	// Ad hoc cloud-init editing (VM only, ctrl+e): overrideSet distinguishes
	// "never opened the editor" from "opened it and saved empty content",
	// since an empty override is still a deliberate choice to send no
	// cloud-init at all rather than "use the name field instead".
	editingCloudInit    bool
	loadingCloudInit    bool
	cloudInitEditor     textarea.Model
	cloudInitOverride   string
	cloudInitOverrideOK bool
}

func newLaunchModel() launchModel {
	ta := textarea.New()
	ta.Placeholder = "#cloud-config\n..."
	return launchModel{
		kind:            "vm",
		form:            newSimpleForm(launchTitle("vm"), launchFields("vm", nil)),
		cloudInitEditor: ta,
	}
}

// launchTitle is the form's title bar, mentioning ctrl+e only for a VM —
// it's a no-op for a container, which has no cloud-init concept at all.
func launchTitle(kind string) string {
	title := "Launch  —  ctrl+k: switch vm/container"
	if kind == "vm" {
		title += ", ctrl+e: edit cloud-init"
	}
	return title
}

// launchFields builds the field set for kind, carrying over values (keyed
// by label) from whatever form the user was previously looking at — so
// switching kind with ctrl+k doesn't throw away shared fields like Name
// or Image, it just shows/hides the ones that don't apply to the other kind.
func launchFields(kind string, values map[string]string) []formField {
	v := func(label, fallback string) string {
		if s, ok := values[label]; ok && s != "" {
			return s
		}
		return fallback
	}

	fields := []formField{
		textField("Name", "defaults to the image ref", v("Name", "")),
		textField("Image", "e.g. ubuntu:24.04, or nginx:alpine", v("Image", "")),
		textField("CPUs", "", v("CPUs", "1")),
		textField("Memory MiB", "", v("Memory MiB", "1024")),
	}

	if kind == "vm" {
		fields = append(fields,
			textField("Disk GiB", "", v("Disk GiB", "8")),
			textField(cloudInitNameField, "a saved library entry — or ctrl+e to edit ad hoc for just this launch", v(cloudInitNameField, "")),
		)
	} else {
		fields = append(fields,
			textField("Env", "K=V,K2=V2", v("Env", "")),
			textField("Volumes", "host:guest[:ro],...", v("Volumes", "")),
		)
	}

	fields = append(fields,
		textField("Intent name (optional)", "", v("Intent name (optional)", "")),
		textField("Role (optional, needs intent)", "", v("Role (optional, needs intent)", "")),
		textField("Ports", "host:guest[/tcp|udp],...", v("Ports", "")),
	)
	return fields
}

// formValues snapshots every field's current text value, keyed by label,
// for launchFields to carry over across a kind switch.
func formValues(f simpleForm) map[string]string {
	values := make(map[string]string, len(f.fields))
	for _, field := range f.fields {
		if field.Kind == fieldText {
			values[field.Label] = field.input.Value()
		}
	}
	return values
}

func (m model) updateLaunch(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case launchStreamMsg:
		if msg.status != "" {
			m.launch.progressLines = appendProgressLine(m.launch.progressLines, msg.status)
		}
		if msg.err != nil {
			m.launch.launching = false
			m.setStatus("launch: "+msg.err.Error(), true)
			return m, nil
		}
		if msg.done {
			m.launch.launching = false
			if msg.instance != nil {
				m.setStatus("launched "+msg.instance.GetName(), false)
				m.screen = screenInstances
				return m, loadInstances(m.client)
			}
			return m, nil
		}
		return m, receiveLaunchEvent(msg.stream)

	case cloudInitContentLoadedMsg:
		if !m.launch.loadingCloudInit {
			return m, nil // stale: the editor was closed before this arrived
		}
		m.launch.loadingCloudInit = false
		content := msg.content
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("loading %q to edit: %v (starting from a blank template instead)", msg.name, msg.err), true)
			content = "#cloud-config\n"
		}
		m.launch.cloudInitEditor.SetValue(content)
		m.launch.cloudInitEditor.Focus()
		return m, nil

	case tea.KeyMsg:
		if m.launch.launching {
			return m, nil // one thing at a time — ignore input mid-launch
		}

		if m.launch.editingCloudInit {
			return m.updateLaunchCloudInitEditor(msg)
		}

		if msg.String() == "ctrl+k" {
			values := formValues(m.launch.form)
			if m.launch.kind == "vm" {
				m.launch.kind = "container"
			} else {
				m.launch.kind = "vm"
			}
			m.launch.form.title = launchTitle(m.launch.kind)
			m.launch.form.fields = launchFields(m.launch.kind, values)
			if m.launch.form.focus >= len(m.launch.form.fields) {
				m.launch.form.focus = 0
			}
			// The new fields are freshly constructed textinput.Models,
			// none of them actually focused yet even though View() will
			// render whichever index m.focus points at as if it were —
			// without this, the form looks focused but silently eats
			// keystrokes until the user tabs away and back.
			m.launch.form.focusCurrent()
			return m, nil
		}
		if msg.String() == "ctrl+e" && m.launch.kind == "vm" {
			return m.startEditingLaunchCloudInit()
		}

		var submitted, cancelled bool
		m.launch.form, submitted, cancelled = m.launch.form.update(msg)
		if cancelled {
			m.screen = screenInstances
			return m, nil
		}
		if submitted {
			req, err := buildLaunchRequest(m.launch)
			if err != nil {
				m.launch.form.errMsg = err.Error()
				return m, nil
			}
			m.launch.launching = true
			m.launch.progressLines = nil
			return m, startLaunchStream(m.client, req)
		}
		return m, nil
	}
	return m, nil
}

// startEditingLaunchCloudInit opens the ad hoc cloud-init editor, seeding
// it from (in priority order) an override already saved this session, the
// named saved config if one's set, or an empty template.
func (m model) startEditingLaunchCloudInit() (tea.Model, tea.Cmd) {
	m.launch.editingCloudInit = true
	if m.launch.cloudInitOverrideOK {
		m.launch.cloudInitEditor.SetValue(m.launch.cloudInitOverride)
		m.launch.cloudInitEditor.Focus()
		return m, nil
	}
	if name := m.launch.form.Value(cloudInitNameField); name != "" {
		m.launch.loadingCloudInit = true
		m.launch.cloudInitEditor.SetValue("")
		return m, loadCloudInitContent(m.client, name)
	}
	m.launch.cloudInitEditor.SetValue("#cloud-config\n")
	m.launch.cloudInitEditor.Focus()
	return m, nil
}

func (m model) updateLaunchCloudInitEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.launch.editingCloudInit = false
		m.launch.loadingCloudInit = false
		m.launch.cloudInitEditor.Blur()
		return m, nil
	case "ctrl+s":
		m.launch.editingCloudInit = false
		m.launch.cloudInitOverride = m.launch.cloudInitEditor.Value()
		m.launch.cloudInitOverrideOK = true
		m.launch.cloudInitEditor.Blur()
		m.setStatus("cloud-init: using the edited content for this launch only", false)
		return m, nil
	}
	if m.launch.loadingCloudInit {
		return m, nil // still fetching — ignore typing until it lands
	}
	var cmd tea.Cmd
	m.launch.cloudInitEditor, cmd = m.launch.cloudInitEditor.Update(msg)
	return m, cmd
}

func (m launchModel) View() string {
	if m.launching {
		s := styleTitle.Render(" Launching… ") + "\n\n"
		for _, line := range m.progressLines {
			s += line + "\n"
		}
		return s
	}
	if m.editingCloudInit {
		body := m.cloudInitEditor.View()
		if m.loadingCloudInit {
			body = styleSubtitle.Render("loading…")
		}
		return styleTitle.Render(" Cloud-init for this launch (not saved to the library) ") + "\n\n" +
			body + "\n" + helpBar("ctrl+s", "use for this launch", "esc", "cancel")
	}
	return styleSubtitle.Render("kind: "+m.kind+cloudInitStatusSuffix(m)) + "\n\n" + m.form.View()
}

// cloudInitStatusSuffix appends a short cloud-init status to the launch
// screen's subtitle line, only meaningful for a VM.
func cloudInitStatusSuffix(m launchModel) string {
	if m.kind != "vm" {
		return ""
	}
	switch {
	case m.cloudInitOverrideOK:
		return styleSubtitle.Render("  •  cloud-init: edited for this launch (ctrl+e to change)")
	case m.form.Value(cloudInitNameField) != "":
		return styleSubtitle.Render("  •  cloud-init: " + m.form.Value(cloudInitNameField))
	default:
		return styleSubtitle.Render("  •  cloud-init: default (ctrl+e to edit)")
	}
}

func buildLaunchRequest(m launchModel) (*anvilv1.LaunchRequest, error) {
	form := m.form
	image := form.Value("Image")
	if image == "" {
		return nil, fmt.Errorf("Image is required")
	}
	name := form.Value("Name")
	if name == "" {
		name = image
	}

	cpus, err := parseInt32(form.Value("CPUs"), "CPUs")
	if err != nil {
		return nil, err
	}
	memMiB, err := parseInt64(form.Value("Memory MiB"), "Memory MiB")
	if err != nil {
		return nil, err
	}
	ports, err := parsePortsField(form.Value("Ports"))
	if err != nil {
		return nil, err
	}

	req := &anvilv1.LaunchRequest{
		Name:       name,
		IntentName: form.Value("Intent name (optional)"),
		Role:       form.Value("Role (optional, needs intent)"),
	}

	if m.kind == "container" {
		req.Kind = anvilv1.Kind_KIND_CONTAINER
		env, err := parseEnvField(form.Value("Env"))
		if err != nil {
			return nil, err
		}
		volumes, err := parseVolumesField(form.Value("Volumes"))
		if err != nil {
			return nil, err
		}
		req.Container = &anvilv1.ContainerSpec{ImageRef: image, Env: env, Volumes: volumes, Ports: ports}
		return req, nil
	}

	req.Kind = anvilv1.Kind_KIND_VM
	diskGiB, err := parseInt64(form.Value("Disk GiB"), "Disk GiB")
	if err != nil {
		return nil, err
	}
	pub, err := sshkey.EnsureDefaultPublic()
	if err != nil {
		return nil, err
	}
	req.Vm = &anvilv1.VMSpec{
		ImageRef:      image,
		Cpus:          cpus,
		MemoryMib:     memMiB,
		DiskGib:       diskGiB,
		SshPublicKeys: []string{pub},
		Ports:         ports,
	}
	// The ad hoc editor, when used, always wins over the saved-library
	// name — editing is meant as "just for this launch", so it shouldn't
	// silently lose to whatever's still sitting in the name field.
	if m.cloudInitOverrideOK {
		req.Vm.CloudInitUserData = m.cloudInitOverride
	} else {
		req.Vm.CloudInitName = form.Value(cloudInitNameField)
	}
	return req, nil
}

// startLaunchStream opens the Launch RPC and receives its first event.
func startLaunchStream(c *client.Client, req *anvilv1.LaunchRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Launch(context.Background(), req)
		if err != nil {
			return launchStreamMsg{err: err, done: true}
		}
		return receiveLaunchEvent(stream)()
	}
}

func receiveLaunchEvent(stream anvilv1.InstanceService_LaunchClient) tea.Cmd {
	return func() tea.Msg {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return launchStreamMsg{done: true}
			}
			return launchStreamMsg{err: err, done: true}
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.LaunchProgress_Status:
			return launchStreamMsg{stream: stream, status: e.Status}
		case *anvilv1.LaunchProgress_Error:
			return launchStreamMsg{err: fmt.Errorf("%s", e.Error), done: true}
		case *anvilv1.LaunchProgress_Instance:
			return launchStreamMsg{instance: e.Instance, done: true}
		default:
			return launchStreamMsg{stream: stream}
		}
	}
}
