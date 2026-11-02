package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func instancesModelWith(insts ...*anvilv1.Instance) model {
	m := model{screen: screenInstances, instances: newInstancesModel()}
	items := make([]list.Item, len(insts))
	for i, inst := range insts {
		items[i] = instanceItem{inst: inst}
	}
	m.instances.list.SetItems(items)
	m.instances.loading = false
	return m
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestInstanceItemShowsIP(t *testing.T) {
	slirp := instanceItem{inst: &anvilv1.Instance{Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{},
		Guest: &anvilv1.GuestInfo{IpAddresses: []string{"10.0.2.15/24"}}}}
	if !strings.Contains(slirp.Description(), "10.0.2.15") {
		t.Errorf("expected the guest IP in %q", slirp.Description())
	}
	bridged := instanceItem{inst: &anvilv1.Instance{Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{StaticIp: "10.55.1.4/24"},
		Guest: &anvilv1.GuestInfo{IpAddresses: []string{"172.17.0.1/16"}}}}
	if !strings.Contains(bridged.Description(), "10.55.1.4") {
		t.Errorf("expected the bridge IP in %q", bridged.Description())
	}
}

func TestUmountPromptSuggestsMounts(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{
		Mounts: []*anvilv1.Mount{{GuestPath: "/mnt/a"}, {GuestPath: "/mnt/b"}},
	}})
	next, _ := m.updateInstancesKey(key("M"))
	m = next.(model)
	if m.instances.prompt != instancesPromptUmount {
		t.Fatal("expected the umount prompt")
	}
	got := m.instances.promptForm.fields[0].Suggestions
	if len(got) != 2 || got[0] != "/mnt/a" || got[1] != "/mnt/b" {
		t.Errorf("expected the mounts as suggestions, got %v", got)
	}
}

func TestDetailShowsMounts(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, Vm: &anvilv1.VMSpec{
		Mounts: []*anvilv1.Mount{{GuestPath: "/mnt/a", HostPath: "/home/me/a", ReadOnly: true}},
	}})
	if view := m.instances.detailView(tasksModel{}); !strings.Contains(view, "/mnt/a ← /home/me/a (ro)") {
		t.Errorf("expected the mount in the detail panel, got:\n%s", view)
	}
}

func TestWaitKeyStartsATask(t *testing.T) {
	stopped := instancesModelWith(&anvilv1.Instance{Name: "off", Kind: anvilv1.Kind_KIND_VM, State: anvilv1.State_STATE_STOPPED})
	next, cmd := stopped.updateInstancesKey(key("w"))
	if len(next.(model).tasks.items) != 0 || cmd != nil {
		t.Error("expected w to do nothing on a stopped instance")
	}

	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, State: anvilv1.State_STATE_RUNNING})
	next, cmd = m.updateInstancesKey(key("w"))
	m = next.(model)
	if len(m.tasks.items) != 1 || cmd == nil {
		t.Fatal("expected w to start a wait task")
	}
	task := m.tasks.items[0]
	if !task.running || task.instance != "web" || task.cancel == nil {
		t.Fatalf("wait task %+v", task)
	}
	if m.tasks.forInstance("web") != task {
		t.Error("expected the task to show on its instance")
	}

	next, _ = m.Update(taskStepMsg{id: task.id, lineStep: lineStep{status: "waiting for cloud-init: running"}})
	m = next.(model)
	if task.last() != "waiting for cloud-init: running" {
		t.Errorf("expected the progress line, got %q", task.last())
	}
	next, _ = m.Update(taskStepMsg{id: task.id, lineStep: lineStep{err: status.Error(codes.Canceled, "context canceled"), done: true}})
	m = next.(model)
	if task.running || !strings.Contains(task.last(), "cancelled") {
		t.Errorf("expected a cancelled task, got running %v, %q", task.running, task.last())
	}
}

func TestWaitFailureIsShown(t *testing.T) {
	m := instancesModelWith(&anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, State: anvilv1.State_STATE_RUNNING})
	next, _ := m.updateInstancesKey(key("w"))
	m = next.(model)
	task := m.tasks.items[0]
	next, _ = m.Update(taskStepMsg{id: task.id, lineStep: lineStep{err: errors.New("cloud-init finished with errors"), done: true}})
	m = next.(model)
	if !strings.Contains(task.last(), "cloud-init finished with errors") || !m.statusBad {
		t.Errorf("expected the error line and status, got %q", task.last())
	}
}

func TestImageChecksumShownOnItem(t *testing.T) {
	m := model{screen: screenImages, images: newImagesModel()}
	m.images.cached.SetItems([]list.Item{cachedImageItem{image: &anvilv1.CachedImage{Id: "ubuntu-24.04", Arch: "x86_64", Kind: "base"}}})
	sum := strings.Repeat("ab", 32)
	next, _ := m.updateImages(imageChecksumMsg{id: "ubuntu-24.04", arch: "x86_64", sum: sum})
	m = next.(model)
	item := m.images.cached.Items()[0].(cachedImageItem)
	if item.sha256 != sum || !strings.Contains(item.Description(), "sha256 abababababab") {
		t.Errorf("expected the checksum on the item, got %q", item.Description())
	}
	if !strings.Contains(m.status, sum) {
		t.Errorf("expected the full checksum in the status line, got %q", m.status)
	}
}

func TestInstanceItemShowsHypervisor(t *testing.T) {
	fc := instanceItem{inst: &anvilv1.Instance{Kind: anvilv1.Kind_KIND_VM,
		Vm: &anvilv1.VMSpec{Hypervisor: anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER}}}
	if !strings.HasPrefix(fc.Description(), "vm/firecracker") {
		t.Errorf("expected the hypervisor in %q", fc.Description())
	}
}
