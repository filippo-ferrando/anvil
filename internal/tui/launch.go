package tui

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
	"github.com/anvil-project/anvil/pkg/client"
)

type launchModel struct {
	kind          string // "vm" | "container"
	form          simpleForm
	launching     bool
	progressLines []string
}

func newLaunchModel() launchModel {
	return launchModel{
		kind: "vm",
		form: newSimpleForm("Launch  —  ctrl+k: switch vm/container", []formField{
			textField("Name", "defaults to the image ref", ""),
			textField("Image", "e.g. ubuntu:24.04, or nginx:alpine", ""),
			textField("CPUs", "", "1"),
			textField("Memory MiB", "", "1024"),
			textField("Disk GiB (VM only)", "", "8"),
			textField("Cloud-init name (VM only, optional)", "", ""),
			textField("Intent name (optional)", "", ""),
			textField("Role (optional, needs intent)", "", ""),
			textField("Env (container only)", "K=V,K2=V2", ""),
			textField("Volumes (container only)", "host:guest[:ro],...", ""),
			textField("Ports", "host:guest[/tcp|udp],...", ""),
		}),
	}
}

func (m model) updateLaunch(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case launchStreamMsg:
		if msg.status != "" {
			m.launch.progressLines = append(m.launch.progressLines, msg.status)
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

	case tea.KeyMsg:
		if m.launch.launching {
			return m, nil // one thing at a time — ignore input mid-launch
		}
		if msg.String() == "ctrl+k" {
			if m.launch.kind == "vm" {
				m.launch.kind = "container"
			} else {
				m.launch.kind = "vm"
			}
			return m, nil
		}

		var submitted, cancelled bool
		m.launch.form, submitted, cancelled = m.launch.form.update(msg)
		if cancelled {
			m.screen = screenInstances
			return m, nil
		}
		if submitted {
			req, err := buildLaunchRequest(m.launch.kind, m.launch.form)
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

func (m launchModel) View() string {
	if m.launching {
		s := styleTitle.Render(" Launching… ") + "\n\n"
		for _, line := range m.progressLines {
			s += line + "\n"
		}
		return s
	}
	return styleSubtitle.Render("kind: "+m.kind) + "\n\n" + m.form.View()
}

func buildLaunchRequest(kind string, form simpleForm) (*anvilv1.LaunchRequest, error) {
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

	if kind == "container" {
		req.Kind = anvilv1.Kind_KIND_CONTAINER
		env, err := parseEnvField(form.Value("Env (container only)"))
		if err != nil {
			return nil, err
		}
		volumes, err := parseVolumesField(form.Value("Volumes (container only)"))
		if err != nil {
			return nil, err
		}
		req.Container = &anvilv1.ContainerSpec{ImageRef: image, Env: env, Volumes: volumes, Ports: ports}
		return req, nil
	}

	req.Kind = anvilv1.Kind_KIND_VM
	diskGiB, err := parseInt64(form.Value("Disk GiB (VM only)"), "Disk GiB")
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
		CloudInitName: form.Value("Cloud-init name (VM only, optional)"),
		SshPublicKeys: []string{pub},
		Ports:         ports,
	}
	return req, nil
}

// startLaunchStream opens the Launch RPC and receives its first event —
// every event after that is chained by launchStreamMsg's own handler in
// updateLaunch re-issuing receiveLaunchEvent, the standard Bubble Tea
// pattern for consuming a gRPC server-streaming call one message at a
// time without blocking the UI loop.
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
