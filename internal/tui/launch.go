package tui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rivo/tview"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
)

// showLaunchForm is the M8 checklist's launch form: a tview.Form overlay
// (not a multi-step wizard, per the plan) covering the fields common to
// both kinds plus each kind's own — env/volumes/ports are a single
// comma-separated text field each rather than a dynamic add/remove row
// list, a deliberate v1 simplification (a real per-row list editor is a
// nicer follow-up, not a blocker for a working form today). onLaunched is
// called after a successful launch, so the caller (instancesView) can
// refresh its table.
func showLaunchForm(app *App, onLaunched func()) {
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Launch ")

	kindOptions := []string{"vm", "container"}
	var kind string
	form.AddInputField("Name", "", 32, nil, nil)
	form.AddDropDown("Kind", kindOptions, 0, func(option string, _ int) { kind = option })
	kind = kindOptions[0]
	form.AddInputField("Image", "", 40, nil, nil)
	form.AddInputField("CPUs", "1", 8, nil, nil)
	form.AddInputField("Memory MiB", "1024", 8, nil, nil)
	form.AddInputField("Disk GiB (VM only)", "8", 8, nil, nil)
	form.AddInputField("Cloud-init name (VM only, optional)", "", 24, nil, nil)
	form.AddInputField("Intent name (optional)", "", 24, nil, nil)
	form.AddInputField("Role (optional, needs Intent)", "", 24, nil, nil)
	form.AddInputField("Env K=V,K2=V2 (container only)", "", 40, nil, nil)
	form.AddInputField("Volumes host:guest[:ro],... (container only)", "", 40, nil, nil)
	form.AddInputField("Ports host:guest[/proto],...", "", 40, nil, nil)

	getField := func(label string) string {
		if item := form.GetFormItemByLabel(label); item != nil {
			if input, ok := item.(*tview.InputField); ok {
				return strings.TrimSpace(input.GetText())
			}
		}
		return ""
	}

	closeForm := func() { app.pages.RemovePage("launch") }

	form.AddButton("Launch", func() {
		req, err := buildLaunchRequest(kind, getField)
		if err != nil {
			app.showError("Launch", err.Error())
			return
		}
		closeForm()
		runLaunch(app, req, onLaunched)
	})
	form.AddButton("Cancel", closeForm)

	modal := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(form, 70, 0, true).
			AddItem(nil, 0, 1, false),
			24, 0, true).
		AddItem(nil, 0, 1, false)

	app.pages.AddPage("launch", modal, true, true)
	app.tapp.SetFocus(form)
}

// buildLaunchRequest translates the form's field text into a
// LaunchRequest, applying the same per-field formats
// internal/cli/commands/launch.go's own flags already use (--env,
// --volume, --publish) — a deliberate, small duplication: the TUI only
// reuses pkg/client, not internal/cli/commands, per the plan's TUI
// section.
func buildLaunchRequest(kind string, field func(string) string) (*anvilv1.LaunchRequest, error) {
	name := field("Name")
	image := field("Image")
	if image == "" {
		return nil, fmt.Errorf("Image is required")
	}
	if name == "" {
		name = image
	}

	cpus, err := parseInt32(field("CPUs"), "CPUs")
	if err != nil {
		return nil, err
	}
	memMiB, err := parseInt64(field("Memory MiB"), "Memory MiB")
	if err != nil {
		return nil, err
	}

	req := &anvilv1.LaunchRequest{
		Name:       name,
		IntentName: field("Intent name (optional)"),
		Role:       field("Role (optional, needs Intent)"),
	}

	ports, err := parsePortsField(field("Ports host:guest[/proto],..."))
	if err != nil {
		return nil, err
	}

	switch kind {
	case "container":
		req.Kind = anvilv1.Kind_KIND_CONTAINER
		env, err := parseEnvField(field("Env K=V,K2=V2 (container only)"))
		if err != nil {
			return nil, err
		}
		volumes, err := parseVolumesField(field("Volumes host:guest[:ro],... (container only)"))
		if err != nil {
			return nil, err
		}
		req.Container = &anvilv1.ContainerSpec{
			ImageRef: image,
			Env:      env,
			Volumes:  volumes,
			Ports:    ports,
		}
	default:
		req.Kind = anvilv1.Kind_KIND_VM
		diskGiB, err := parseInt64(field("Disk GiB (VM only)"), "Disk GiB")
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
			CloudInitName: field("Cloud-init name (VM only, optional)"),
			SshPublicKeys: []string{pub},
			Ports:         ports,
		}
	}
	return req, nil
}

// runLaunch drives the streaming Launch RPC in the background, showing a
// progress modal that updates as events arrive and closes itself on
// success or failure.
func runLaunch(app *App, req *anvilv1.LaunchRequest, onLaunched func()) {
	progress := tview.NewTextView().SetDynamicColors(true).SetText("starting…")
	progress.SetBorder(true).SetTitle(" Launching " + req.GetName() + " ")
	modal := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(progress, 60, 0, true).
			AddItem(nil, 0, 1, false),
			10, 0, true).
		AddItem(nil, 0, 1, false)
	app.pages.AddPage("launch-progress", modal, true, true)

	go func() {
		stream, err := app.client.Launch(context.Background(), req)
		if err != nil {
			app.tapp.QueueUpdateDraw(func() {
				app.pages.RemovePage("launch-progress")
				app.showError("Launch", err.Error())
			})
			return
		}
		for {
			ev, err := stream.Recv()
			if err != nil {
				app.tapp.QueueUpdateDraw(func() {
					app.pages.RemovePage("launch-progress")
					if err != io.EOF {
						app.showError("Launch", err.Error())
					}
				})
				return
			}
			switch e := ev.GetEvent().(type) {
			case *anvilv1.LaunchProgress_Status:
				app.tapp.QueueUpdateDraw(func() { progress.SetText(e.Status) })
			case *anvilv1.LaunchProgress_Error:
				app.tapp.QueueUpdateDraw(func() {
					app.pages.RemovePage("launch-progress")
					app.showError("Launch", e.Error)
				})
				return
			case *anvilv1.LaunchProgress_Instance:
				app.tapp.QueueUpdateDraw(func() {
					app.pages.RemovePage("launch-progress")
					onLaunched()
				})
				return
			}
		}
	}()
}

func parseInt32(s, field string) (int32, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return int32(n), nil
}

func parseInt64(s, field string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return n, nil
}

func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseEnvField(s string) (map[string]string, error) {
	items := splitCommaList(s)
	if len(items) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(items))
	for _, v := range items {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("env %q must be in KEY=VALUE form", v)
		}
		env[k] = val
	}
	return env, nil
}

func parseVolumesField(s string) ([]*anvilv1.VolumeMount, error) {
	var out []*anvilv1.VolumeMount
	for _, v := range splitCommaList(s) {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf(`volume %q must be "<host-path>:<container-path>[:ro]"`, v)
		}
		readOnly := false
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf(`volume %q: third part must be "ro"`, v)
			}
			readOnly = true
		}
		out = append(out, &anvilv1.VolumeMount{HostPath: parts[0], ContainerPath: parts[1], ReadOnly: readOnly})
	}
	return out, nil
}

func parsePortsField(s string) ([]*anvilv1.PortMapping, error) {
	var out []*anvilv1.PortMapping
	for _, p := range splitCommaList(s) {
		spec, protocol := p, "tcp"
		if host, proto, ok := strings.Cut(p, "/"); ok {
			spec, protocol = host, proto
		}
		hostStr, guestStr, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf(`port %q must be "<host-port>:<guest-port>[/tcp|udp]"`, p)
		}
		hostPort, err := strconv.Atoi(hostStr)
		if err != nil {
			return nil, fmt.Errorf("port %q: invalid host port: %w", p, err)
		}
		guestPort, err := strconv.Atoi(guestStr)
		if err != nil {
			return nil, fmt.Errorf("port %q: invalid guest port: %w", p, err)
		}
		out = append(out, &anvilv1.PortMapping{HostPort: int32(hostPort), GuestPort: int32(guestPort), Protocol: protocol})
	}
	return out, nil
}
