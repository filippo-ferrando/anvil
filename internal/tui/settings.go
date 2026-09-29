package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

// restartPolicySuggestions are the values offered for every restart policy field.
var restartPolicySuggestions = []string{"no", "on-failure", "on-failure:5", "always"}

const (
	settingsCPUs      = "CPUs"
	settingsMemory    = "Memory MiB"
	settingsDisk      = "Disk GiB"
	settingsAutostart = "Autostart"
	settingsRestart   = "Restart policy"
)

// settingsForm is the "u" prompt, pre-filled with inst's current settings.
func settingsForm(inst *anvilv1.Instance) simpleForm {
	var fields []formField
	if vm := inst.GetVm(); vm != nil {
		disk := ""
		if vm.GetDiskGib() > 0 {
			disk = strconv.FormatInt(vm.GetDiskGib(), 10)
		}
		fields = append(fields,
			textField(settingsCPUs, "changes live on a running VM when it can", strconv.Itoa(int(vm.GetCpus()))),
			textField(settingsMemory, "changes live on a running VM, not below what it booted with", strconv.FormatInt(vm.GetMemoryMib(), 10)),
			textField(settingsDisk, "grow only; applied live, empty keeps the current size", disk),
		)
	}
	restart := textField(settingsRestart, "no, on-failure[:N] or always", restartPolicyText(inst.GetRestartPolicy()))
	restart.Suggestions = restartPolicySuggestions
	fields = append(fields,
		toggleField(settingsAutostart, "start it whenever anvild starts, unless stopped on purpose", inst.GetAutostart()),
		restart,
	)
	return newSimpleForm("Settings for "+inst.GetName(), fields)
}

// buildUpdateRequest turns the settings form into an UpdateRequest holding only what changed.
// changed is false when nothing did.
func buildUpdateRequest(inst *anvilv1.Instance, form simpleForm) (req *anvilv1.UpdateRequest, changed bool, err error) {
	req = &anvilv1.UpdateRequest{Name: inst.GetName()}
	if vm := inst.GetVm(); vm != nil {
		cpus, err := parseInt32(form.Value(settingsCPUs), settingsCPUs)
		if err != nil {
			return nil, false, err
		}
		if cpus != 0 && cpus != vm.GetCpus() {
			req.Cpus = &cpus
		}
		mem, err := parseInt64(form.Value(settingsMemory), settingsMemory)
		if err != nil {
			return nil, false, err
		}
		if mem != 0 && mem != vm.GetMemoryMib() {
			req.MemoryMib = &mem
		}
		disk, err := parseInt64(form.Value(settingsDisk), settingsDisk)
		if err != nil {
			return nil, false, err
		}
		if disk != 0 && disk != vm.GetDiskGib() {
			req.DiskGib = &disk
		}
	}
	if auto := form.Bool(settingsAutostart); auto != inst.GetAutostart() {
		req.Autostart = &auto
	}
	if p := strings.TrimSpace(form.Value(settingsRestart)); p != "" && p != restartPolicyText(inst.GetRestartPolicy()) {
		req.RestartPolicy = &anvilv1.RestartPolicy{Mode: p}
	}
	changed = req.Cpus != nil || req.MemoryMib != nil || req.DiskGib != nil || req.Autostart != nil || req.RestartPolicy != nil
	return req, changed, nil
}

// restartPolicyText renders p the way the restart policy field takes it.
func restartPolicyText(p *anvilv1.RestartPolicy) string {
	mode := p.GetMode()
	if mode == "" {
		mode = "no"
	}
	if p.GetMaxRetries() > 0 {
		return fmt.Sprintf("%s:%d", mode, p.GetMaxRetries())
	}
	return mode
}

// updateInstance sends req and reports the daemon's notes on the status line.
func updateInstance(c *client.Client, req *anvilv1.UpdateRequest) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Update(context.Background(), req)
		verb := "nothing changed"
		if err == nil && len(reply.GetNotes()) > 0 {
			verb = strings.Join(reply.GetNotes(), "; ")
		}
		return actionDoneMsg{screen: screenInstances, verb: verb, err: err}
	}
}

// resourcesText summarizes a VM's size for the detail panel, e.g. "2 vCPU • 2048 MiB • 20 GiB".
func resourcesText(vm *anvilv1.VMSpec) string {
	disk := "default disk"
	if vm.GetDiskGib() > 0 {
		disk = fmt.Sprintf("%d GiB", vm.GetDiskGib())
	}
	return fmt.Sprintf("%d vCPU  •  %d MiB  •  %s", vm.GetCpus(), vm.GetMemoryMib(), disk)
}

// scheduleText summarizes a snapshot schedule, or "" when there is none.
func scheduleText(s *anvilv1.SnapshotSchedule) string {
	if s.GetEverySeconds() <= 0 {
		return ""
	}
	out := fmt.Sprintf("every %s, keep %d", time.Duration(s.GetEverySeconds())*time.Second, s.GetKeep())
	if s.GetLastRunUnix() > 0 {
		out += ", last " + time.Unix(s.GetLastRunUnix(), 0).Format("01-02 15:04")
	}
	return out
}

// scheduleForm is the Snapshots screen's "S" prompt, pre-filled with inst's schedule.
func scheduleForm(inst *anvilv1.Instance) simpleForm {
	every, keep := "", "7"
	if s := inst.GetVm().GetSnapshotSchedule(); s.GetEverySeconds() > 0 {
		every = (time.Duration(s.GetEverySeconds()) * time.Second).String()
		keep = strconv.Itoa(int(s.GetKeep()))
	}
	return newSimpleForm("Snapshot schedule for "+inst.GetName(), []formField{
		textField("Every", "e.g. 6h or 30m; empty turns the schedule off", every),
		textField("Keep", "how many scheduled snapshots to keep", keep),
	})
}

// buildScheduleRequest reads the schedule form; an empty interval removes the schedule.
func buildScheduleRequest(inst *anvilv1.Instance, form simpleForm) (*anvilv1.SnapshotSetScheduleRequest, error) {
	req := &anvilv1.SnapshotSetScheduleRequest{Name: inst.GetName()}
	every := strings.TrimSpace(form.Value("Every"))
	if every == "" || every == "0" {
		return req, nil
	}
	d, err := time.ParseDuration(every)
	if err != nil {
		return nil, fmt.Errorf("Every: %w", err)
	}
	keep, err := parseInt32(form.Value("Keep"), "Keep")
	if err != nil {
		return nil, err
	}
	req.EverySeconds, req.Keep = int64(d/time.Second), keep
	return req, nil
}

func setSnapshotSchedule(c *client.Client, req *anvilv1.SnapshotSetScheduleRequest) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Snapshot.SetSchedule(context.Background(), req)
		verb := "snapshot schedule removed"
		if req.GetEverySeconds() > 0 {
			verb = "snapshot schedule set"
		}
		return actionDoneMsg{screen: screenSnapshots, verb: verb, err: err}
	}
}
