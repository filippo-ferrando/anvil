package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// cloudInitNameField is the VM-only field naming a saved library entry.
// It's shared between newLaunchModel/launchFields and buildLaunchRequest so they can't drift out of sync.
const cloudInitNameField = "Cloud-init name (optional)"

// Field labels shared the same way as cloudInitNameField.
const (
	guestAgentField  = "Guest agent"
	hypervisorField  = "Hypervisor"
	sshKeysField     = "Extra SSH keys"
	entrypointField  = "Entrypoint"
	commandField     = "Command"
	startField       = "Start"
	waitField        = "Wait until ready"
	waitTimeoutField = "Wait timeout"
)

type launchModel struct {
	kind          string // "vm" | "container"
	form          simpleForm
	launching     bool
	progressLines []string
	stepSince     time.Time // when the last progress line's step began

	// Ad hoc cloud-init editing (VM only, ctrl+e): distinguishes "never
	// opened the editor" from "opened it and saved empty content", since an empty override is still a deliberate choice.
	editingCloudInit    bool
	loadingCloudInit    bool
	cloudInitEditor     yamlEditor
	cloudInitOverride   string
	cloudInitOverrideOK bool

	// suggest holds Tab-completion candidates gathered from the other
	// screens' data, (re)fetched fresh each time Launch is entered.
	suggest launchSuggestions

	// devices are the host's passthrough candidates; picked keeps checked IDs across a field rebuild.
	devices []*anvilv1.HostDevice
	picked  map[string]bool
}

// Labels of the two device drop-downs a qemu VM's launch form ends with.
const (
	usbDevicesField = "USB devices"
	pciDevicesField = "PCI devices"
)

// wantsDevices is true when the form describes a VM that can take passthrough devices.
func (m launchModel) wantsDevices() bool {
	return m.kind == "vm" && strings.TrimSpace(m.form.Value(hypervisorField)) != "firecracker"
}

// syncDeviceFields keeps the USB and PCI drop-downs at the end of the form, only for a qemu VM.
// It runs on every key, so an existing drop-down keeps its open state and cursor.
func (m *launchModel) syncDeviceFields() {
	if m.picked == nil {
		m.picked = make(map[string]bool)
	}
	old := make(map[string]formField)
	fields := m.form.fields[:0]
	for _, f := range m.form.fields {
		if f.Kind == fieldPicker {
			old[f.Label] = f
		} else {
			fields = append(fields, f)
		}
	}
	if m.wantsDevices() && len(m.devices) > 0 {
		for _, bus := range []struct{ name, label string }{{"usb", usbDevicesField}, {"pci", pciDevicesField}} {
			var opts []pickOption
			for _, d := range m.devices {
				if d.GetBus() != bus.name {
					continue
				}
				note := ""
				if st := client.DeviceStatus(d); st != "free" {
					note = st
				}
				opts = append(opts, pickOption{ID: d.GetId(), Label: d.GetId() + "  " + deviceName(d), Note: note})
			}
			f := pickerField(bus.label, "", opts, m.picked)
			if o, ok := old[bus.label]; ok {
				f.open, f.cursor = o.open, min(o.cursor, max(len(opts)-1, 0))
			}
			fields = append(fields, f)
		}
	}
	m.form.fields = fields
	if m.form.focus >= len(fields) {
		m.form.focus = len(fields) - 1
		m.form.focusCurrent()
	}
}

// launchSuggestions holds autocomplete candidates for the launch form.
type launchSuggestions struct {
	vmImages        []string // catalog ids ∪ already-cached ids
	fcImages        []string // the subset of vmImages Firecracker can boot
	containerImages []string // repo:tag refs of already-pulled images
	intents         []string // existing intent names
	cloudInits      []string // saved cloud-init library entries
	roles           []string // roles already in use across every intent's members
}

// imagesFor returns the Image-field candidates relevant to kind: a VM's
// image ref and a container's image ref come from different namespaces (a vault catalog id vs. a Docker repo:tag).
func (s launchSuggestions) imagesFor(kind, hypervisor string) []string {
	if kind == "container" {
		return s.containerImages
	}
	if strings.TrimSpace(hypervisor) == "firecracker" {
		return s.fcImages
	}
	return s.vmImages
}

func newLaunchModel() launchModel {
	return launchModel{
		kind:            "vm",
		form:            newSimpleForm(launchTitle("vm"), launchFields("vm", nil)),
		cloudInitEditor: newYAMLEditor("#cloud-config\n...", "valid cloud-config", checkCloudInit),
	}
}

// setSize must run on a real terminal resize and every time a fresh
// launchModel replaces the old one: a new textarea.Model starts zero-sized and stays tiny until this calls SetWidth/SetHeight on it.
func (m *launchModel) setSize(width, height int) {
	m.form.SetHeight(height - 2)
	m.cloudInitEditor.SetWidth(width - 4)
	m.cloudInitEditor.SetHeight(height - 2)
}

// launchTitle is the form's title bar. It mentions ctrl+e only for a VM;
// a container has no cloud-init concept, so the hint would be a no-op there.
func launchTitle(kind string) string {
	title := "Launch (ctrl+k: switch vm/container)"
	if kind == "vm" {
		title += ", ctrl+e: edit cloud-init"
	}
	return title
}

// launchFields builds the field set for kind, carrying over values (keyed
// by label) so switching kind with ctrl+k doesn't lose shared fields like Name or Image, it just shows/hides what doesn't apply.
func launchFields(kind string, values map[string]string) []formField {
	v := func(label, fallback string) string {
		if s, ok := values[label]; ok && s != "" {
			return s
		}
		return fallback
	}

	fields := []formField{
		textField("Name", "defaults to a name derived from the image", v("Name", "")),
		textField("Image", "e.g. ubuntu:24.04, or nginx:alpine", v("Image", "")),
	}

	waitHint := "keep the progress view open until the container is ready"
	if kind == "vm" {
		hypervisor := textField(hypervisorField, "qemu or firecracker", v(hypervisorField, "qemu"))
		hypervisor.Suggestions = []string{"qemu", "firecracker"}
		fields = append(fields,
			textField("CPUs", "", v("CPUs", "1")),
			textField("Memory MiB", "", v("Memory MiB", "1024")),
			hypervisor,
			textField("Disk GiB", "0 = image minimum", v("Disk GiB", "0")),
			textField(cloudInitNameField, "a saved library entry, or ctrl+e to edit ad hoc for just this launch", v(cloudInitNameField, "")),
			textField(sshKeysField, "on top of anvil's own key: literal keys or .pub file paths, comma-separated", v(sshKeysField, "")),
			toggleField(guestAgentField, "install qemu-guest-agent: guest IPs, clean shutdown, cloud-init status", true),
		)
		waitHint = "keep the progress view open until cloud-init finished"
	} else {
		fields = append(fields,
			textField("Env", "K=V,K2=V2", v("Env", "")),
			textField("Volumes", "host:guest[:ro],...", v("Volumes", "")),
			textField(entrypointField, "overrides the image's entrypoint", v(entrypointField, "")),
			textField(commandField, "overrides the image's command, e.g. sleep infinity", v(commandField, "")),
		)
	}

	fields = append(fields,
		toggleField(startField, "start it right after creating it", true),
		toggleField(waitField, waitHint, false),
		textField(waitTimeoutField, "how long the wait may take, e.g. 15m", v(waitTimeoutField, "15m")),
	)

	restart := textField(settingsRestart, "no, on-failure[:N] or always", v(settingsRestart, "no"))
	restart.Suggestions = restartPolicySuggestions
	fields = append(fields,
		toggleField(settingsAutostart, "start it whenever anvild starts, unless stopped on purpose", false),
		restart,
		textField("Intent name (optional)", "", v("Intent name (optional)", "")),
		textField("Role (optional, needs intent)", "", v("Role (optional, needs intent)", "")),
		textField("Ports", "host:guest[/tcp|udp],...", v("Ports", "")),
	)
	return fields
}

// applySuggestions pushes m.suggest's current candidate lists into the
// already-built fields in place: reconstructing them via launchFields instead would cost the user's in-progress typing and focus on every load reply.
func (m *launchModel) applySuggestions() {
	m.setFieldSuggestions("Image", m.suggest.imagesFor(m.kind, m.form.Value(hypervisorField)))
	m.setFieldSuggestions("Intent name (optional)", m.suggest.intents)
	m.setFieldSuggestions(cloudInitNameField, m.suggest.cloudInits)
	m.setFieldSuggestions("Role (optional, needs intent)", m.suggest.roles)
}

func (m *launchModel) setFieldSuggestions(label string, options []string) {
	for i := range m.form.fields {
		if m.form.fields[i].Label == label {
			m.form.fields[i].Suggestions = options
		}
	}
}

// collectRoles gathers the distinct, non-empty roles already used across
// every intent's members, so a new instance can reuse an existing role name instead of typing a near-duplicate.
func collectRoles(intents []*anvilv1.Intent) []string {
	seen := make(map[string]bool)
	var roles []string
	for _, it := range intents {
		for _, mem := range it.GetMembers() {
			if r := mem.GetRole(); r != "" && !seen[r] {
				seen[r] = true
				roles = append(roles, r)
			}
		}
	}
	return roles
}

// mergeUnique appends add's not-already-present entries to existing.
// vmImages is filled by two independent loads (catalog + cached) that can land in either order, so neither may clobber what the other set.
func mergeUnique(existing, add []string) []string {
	seen := make(map[string]bool, len(existing))
	out := append([]string(nil), existing...)
	for _, s := range existing {
		seen[s] = true
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
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
			m.launch.stepSince = stepStart(m.launch.progressLines, msg.status, m.launch.stepSince)
			m.launch.progressLines = appendProgressLine(m.launch.progressLines, msg.status)
		}
		if msg.err != nil {
			m.launch.launching = false
			m.setStatus("launch: "+msg.err.Error(), true)
			return m, nil
		}
		if msg.done {
			m.launch.launching = false
			if msg.result != nil {
				m.setStatus("launched "+msg.result.GetName(), false)
				m.screen = screenInstances
				return m, tea.Batch(tea.ClearScreen, loadInstances(m.client))
			}
			return m, nil
		}
		return m, msg.next

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

	// Each load fired when Launch was entered fills in one slice of
	// autocomplete candidates as its RPC comes back, independently.
	case intentsLoadedMsg:
		if msg.err == nil {
			names := make([]string, 0, len(msg.intents))
			for _, it := range msg.intents {
				names = append(names, it.GetName())
			}
			m.launch.suggest.intents = names
			m.launch.suggest.roles = collectRoles(msg.intents)
			m.launch.applySuggestions()
		}
		return m, nil
	case cloudInitListLoadedMsg:
		if msg.err == nil {
			names := make([]string, 0, len(msg.configs))
			for _, c := range msg.configs {
				names = append(names, c.GetName())
			}
			m.launch.suggest.cloudInits = names
			m.launch.applySuggestions()
		}
		return m, nil
	case catalogLoadedMsg:
		if msg.err == nil {
			var ids, fc []string
			for _, e := range msg.entries {
				ids = append(ids, e.GetId())
				if e.GetFirecracker() {
					fc = append(fc, e.GetId())
				}
			}
			m.launch.suggest.vmImages = mergeUnique(m.launch.suggest.vmImages, ids)
			m.launch.suggest.fcImages = mergeUnique(m.launch.suggest.fcImages, fc)
			m.launch.applySuggestions()
		}
		return m, nil
	case cachedImagesLoadedMsg:
		if msg.err == nil {
			var ids, fc []string
			for _, im := range msg.images {
				switch im.GetKind() {
				case "firecracker-kernel":
					// A kernel file isn't a launchable image.
				case "firecracker-raw":
					fc = append(fc, im.GetId())
				default:
					ids = append(ids, im.GetId())
				}
			}
			m.launch.suggest.vmImages = mergeUnique(m.launch.suggest.vmImages, ids)
			m.launch.suggest.fcImages = mergeUnique(m.launch.suggest.fcImages, fc)
			m.launch.applySuggestions()
		}
		return m, nil
	case hostDevicesLoadedMsg:
		if msg.err == nil {
			m.launch.devices = msg.devices
			m.launch.syncDeviceFields()
		}
		return m, nil
	case containerImagesLoadedMsg:
		if msg.err == nil {
			var refs []string
			for _, im := range msg.images {
				refs = append(refs, im.GetRepoTags()...)
			}
			m.launch.suggest.containerImages = refs
			m.launch.applySuggestions()
		}
		return m, nil

	case tea.KeyMsg:
		if m.launch.launching {
			return m, nil // one thing at a time: ignore input mid-launch
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
			m.launch.form.tabField = -1 // stale index into the fields slice we just replaced
			m.launch.applySuggestions() // the new fields start with no Suggestions of their own
			m.launch.syncDeviceFields()
			if m.launch.form.focus >= len(m.launch.form.fields) {
				m.launch.form.focus = 0
			}
			// The new fields are freshly constructed textinput.Models, none
			// actually focused yet even though View() renders as if one were: without this the form silently eats keystrokes until tab away and back.
			m.launch.form.focusCurrent()
			return m, nil
		}
		if msg.String() == "ctrl+e" && m.launch.kind == "vm" {
			return m.startEditingLaunchCloudInit()
		}

		var submitted, cancelled bool
		m.launch.form, submitted, cancelled = m.launch.form.update(msg)
		m.launch.applySuggestions() // the Image candidates follow the Hypervisor field
		m.launch.syncDeviceFields() // and so does the device picker
		if cancelled {
			m.screen = screenInstances
			return m, tea.ClearScreen
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

// startEditingLaunchCloudInit opens the ad hoc cloud-init editor, seeding it
// from (in priority order) a session override, the named saved config, or an empty template.
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
		return m, nil // still fetching: ignore typing until it lands
	}
	var cmd tea.Cmd
	m.launch.cloudInitEditor, cmd = m.launch.cloudInitEditor.Update(msg)
	return m, cmd
}

func (m launchModel) View() string {
	if m.launching {
		return styleTitle.Render(" Launching… ") + "\n\n" + progressView(m.progressLines, true, m.stepSince)
	}
	if m.editingCloudInit {
		body := m.cloudInitEditor.View()
		if m.loadingCloudInit {
			body = styleSubtitle.Render("loading…")
		}
		return styleTitle.Render(" Cloud-init for this launch (not saved to the library) ") + "\n\n" +
			body + "\n" + helpBar("ctrl+s", "use for this launch", "esc", "cancel")
	}
	head := styleSubtitle.Render("kind: " + m.kind + cloudInitStatusSuffix(m))
	return head + "\n\n" + m.form.View()
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
		return nil, errors.New("image is required")
	}
	name := form.Value("Name")

	ports, err := client.ParsePorts(splitCommaList(form.Value("Ports")))
	if err != nil {
		return nil, err
	}
	start, wait := form.Bool(startField), form.Bool(waitField)
	if wait && !start {
		return nil, errors.New("wait needs Start on")
	}
	waitTimeout, err := parseDurationField(form.Value(waitTimeoutField), waitTimeoutField)
	if err != nil {
		return nil, err
	}

	req := &anvilv1.LaunchRequest{
		Name:               name,
		IntentName:         form.Value("Intent name (optional)"),
		Role:               form.Value("Role (optional, needs intent)"),
		Autostart:          form.Bool(settingsAutostart),
		NoStart:            !start,
		Wait:               wait,
		WaitTimeoutSeconds: client.CeilSeconds(waitTimeout),
	}
	if p := strings.TrimSpace(form.Value(settingsRestart)); p != "" && p != "no" {
		req.RestartPolicy = &anvilv1.RestartPolicy{Mode: p}
	}

	if m.kind == "container" {
		req.Kind = anvilv1.Kind_KIND_CONTAINER
		env, err := client.ParseEnv(splitCommaList(form.Value("Env")))
		if err != nil {
			return nil, err
		}
		volumes, err := client.ParseVolumes(splitCommaList(form.Value("Volumes")))
		if err != nil {
			return nil, err
		}
		req.Container = &anvilv1.ContainerSpec{
			ImageRef: image, Env: env, Volumes: volumes, Ports: ports,
			Cmd: strings.Fields(form.Value(commandField)),
		}
		if ep := form.Value(entrypointField); ep != "" {
			req.Container.Entrypoint = []string{ep}
		}
		return req, nil
	}

	req.Kind = anvilv1.Kind_KIND_VM
	cpus, err := parseInt32(form.Value("CPUs"), "CPUs")
	if err != nil {
		return nil, err
	}
	memMiB, err := parseInt64(form.Value("Memory MiB"), "Memory MiB")
	if err != nil {
		return nil, err
	}
	diskGiB, err := parseInt64(form.Value("Disk GiB"), "Disk GiB")
	if err != nil {
		return nil, err
	}
	hypervisor, err := parseHypervisorField(form.Value(hypervisorField))
	if err != nil {
		return nil, err
	}
	keys, err := client.ResolveSSHKeys(splitCommaList(form.Value(sshKeysField)))
	if err != nil {
		return nil, err
	}
	req.Vm = &anvilv1.VMSpec{
		ImageRef:      image,
		Cpus:          cpus,
		MemoryMib:     memMiB,
		DiskGib:       diskGiB,
		SshPublicKeys: keys,
		Ports:         ports,
		NoGuestAgent:  !form.Bool(guestAgentField),
		Hypervisor:    hypervisor,
	}
	req.Vm.Devices = append(form.Picked(usbDevicesField), form.Picked(pciDevicesField)...)
	// The ad hoc editor, when used, always wins over the saved-library name:
	// editing is meant as "just for this launch" and shouldn't silently lose to whatever's still in the name field.
	if m.cloudInitOverrideOK {
		req.Vm.CloudInitUserData = m.cloudInitOverride
	} else {
		req.Vm.CloudInitName = form.Value(cloudInitNameField)
	}
	return req, nil
}

func parseHypervisorField(s string) (anvilv1.VmHypervisor, error) {
	switch strings.TrimSpace(s) {
	case "", "qemu":
		return anvilv1.VmHypervisor_VM_HYPERVISOR_QEMU, nil
	case "firecracker":
		return anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER, nil
	default:
		return 0, errors.New("hypervisor must be qemu or firecracker")
	}
}

// startLaunchStream opens the Launch RPC and receives its first event.
func startLaunchStream(c *client.Client, req *anvilv1.LaunchRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Launch(context.Background(), req)
		if err != nil {
			return launchStreamMsg{failed[*anvilv1.Instance](err)}
		}
		return recvStream(stream.Recv, parseLaunchProgress, func(s instanceStep) tea.Msg { return launchStreamMsg{s} })()
	}
}
