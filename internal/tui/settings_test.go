package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// setField replaces a text field's value, the way typing into it would.
func setField(f *simpleForm, label, value string) {
	for i := range f.fields {
		if f.fields[i].Label == label {
			f.fields[i].input.SetValue(value)
		}
	}
}

func toggle(f *simpleForm, label string) {
	for i := range f.fields {
		if f.fields[i].Label == label {
			f.fields[i].on = !f.fields[i].on
		}
	}
}

func TestSettingsFormSendsOnlyChanges(t *testing.T) {
	inst := &anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM,
		Vm: &anvilv1.VMSpec{Cpus: 2, MemoryMib: 2048}, RestartPolicy: &anvilv1.RestartPolicy{Mode: "no"}}

	form := settingsForm(inst)
	if _, changed, err := buildUpdateRequest(inst, form); err != nil || changed {
		t.Fatalf("expected an untouched form to change nothing, got changed=%v err=%v", changed, err)
	}

	setField(&form, settingsMemory, "4096")
	setField(&form, settingsDisk, "30")
	setField(&form, settingsRestart, "on-failure:3")
	toggle(&form, settingsAutostart)
	req, changed, err := buildUpdateRequest(inst, form)
	if err != nil || !changed {
		t.Fatalf("expected changes, got changed=%v err=%v", changed, err)
	}
	if req.Cpus != nil {
		t.Error("expected unchanged cpus not to be sent")
	}
	if req.GetMemoryMib() != 4096 || req.GetDiskGib() != 30 || !req.GetAutostart() || req.GetRestartPolicy().GetMode() != "on-failure:3" {
		t.Errorf("unexpected request %+v", req)
	}

	setField(&form, settingsCPUs, "many")
	if _, _, err := buildUpdateRequest(inst, form); err == nil {
		t.Error("expected a parse error for a non-numeric cpu count")
	}
}

func TestSettingsFormForContainerHasNoResources(t *testing.T) {
	form := settingsForm(&anvilv1.Instance{Name: "nginx", Kind: anvilv1.Kind_KIND_CONTAINER, Container: &anvilv1.ContainerSpec{}})
	for _, f := range form.fields {
		if f.Label == settingsCPUs || f.Label == settingsMemory || f.Label == settingsDisk {
			t.Errorf("expected no %q field for a container", f.Label)
		}
	}
}

func TestSettingsKeyOpensPrompt(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{Cpus: 1}})
	next, _ := m.updateInstancesKey(key("u"))
	if next.(model).instances.prompt != instancesPromptSettings {
		t.Error("expected u to open the settings prompt")
	}
}

func TestDetailShowsPolicies(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, Autostart: true,
		RestartPolicy: &anvilv1.RestartPolicy{Mode: "always"},
		Vm:            &anvilv1.VMSpec{Cpus: 2, MemoryMib: 2048, DiskGib: 20, SnapshotSchedule: &anvilv1.SnapshotSchedule{EverySeconds: 3600, Keep: 4}}})
	view := m.instances.detailView()
	for _, want := range []string{"2 vCPU", "2048 MiB", "20 GiB", "autostart", "restart always", "every 1h0m0s, keep 4"} {
		if !strings.Contains(view, want) {
			t.Errorf("expected %q in the detail panel:\n%s", want, view)
		}
	}
}

func TestScheduleForm(t *testing.T) {
	inst := &anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{}}
	form := scheduleForm(inst)
	setField(&form, "Every", "6h")
	setField(&form, "Keep", "8")
	req, err := buildScheduleRequest(inst, form)
	if err != nil || req.GetEverySeconds() != 6*3600 || req.GetKeep() != 8 {
		t.Fatalf("unexpected request %+v, %v", req, err)
	}
	setField(&form, "Every", "")
	if req, _ := buildScheduleRequest(inst, form); req.GetEverySeconds() != 0 {
		t.Error("expected an empty interval to remove the schedule")
	}
	setField(&form, "Every", "soon")
	if _, err := buildScheduleRequest(inst, form); err == nil {
		t.Error("expected a parse error")
	}
}

func TestSnapshotsScheduleKeyAndHeader(t *testing.T) {
	m := model{screen: screenSnapshots, snapshots: newSnapshotsModel()}
	inst := &anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM,
		Vm: &anvilv1.VMSpec{SnapshotSchedule: &anvilv1.SnapshotSchedule{EverySeconds: 1800, Keep: 3}}}
	m.snapshots.instances.SetItems([]list.Item{instanceItem{inst: inst}})
	if view := m.snapshots.View(); !strings.Contains(view, "schedule: every 30m0s, keep 3") {
		t.Errorf("expected the schedule above the snapshots:\n%s", view)
	}
	next, _ := m.updateSnapshotsKey(key("S"))
	sn := next.(model).snapshots
	if sn.prompt != snapshotsPromptSchedule || sn.promptForm.Value("Every") != "30m0s" {
		t.Errorf("expected a pre-filled schedule prompt, got prompt=%v every=%q", sn.prompt, sn.promptForm.Value("Every"))
	}
}

func TestLaunchFormPolicies(t *testing.T) {
	for _, kind := range []string{"vm", "container"} {
		form := newSimpleForm("", launchFields(kind, nil))
		if form.Bool(settingsAutostart) || form.Value(settingsRestart) != "no" {
			t.Errorf("%s: expected autostart off and restart no by default", kind)
		}
	}
}
