package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

type hostDevicesLoadedMsg struct {
	devices []*anvilv1.HostDevice
	err     error
}

func loadHostDevices(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Host.ListHostDevices(context.Background(), &anvilv1.ListHostDevicesRequest{})
		if err != nil {
			return hostDevicesLoadedMsg{err: err}
		}
		return hostDevicesLoadedMsg{devices: reply.GetDevices()}
	}
}

// deviceStatus is client.DeviceStatus plus the IOMMU group members a PCI device brings along.
func deviceStatus(d *anvilv1.HostDevice) string {
	s := client.DeviceStatus(d)
	if len(d.GetGroupMembers()) > 0 {
		s += " (with " + strings.Join(d.GetGroupMembers(), ", ") + ")"
	}
	return s
}

// deviceName falls back to vendor:product when no usb.ids/pci.ids name is known.
func deviceName(d *anvilv1.HostDevice) string {
	if d.GetName() != "" {
		return d.GetName()
	}
	return d.GetVendorId() + ":" + d.GetProductId()
}

type deviceItem struct{ dev *anvilv1.HostDevice }

func (i deviceItem) FilterValue() string { return i.dev.GetId() }
func (i deviceItem) Title() string       { return i.dev.GetId() + "  " + deviceName(i.dev) }
func (i deviceItem) Description() string {
	driver := i.dev.GetDriver()
	if driver == "" {
		driver = "no driver"
	}
	return i.dev.GetVendorId() + ":" + i.dev.GetProductId() + "  •  " + driver + "  •  " + deviceStatus(i.dev)
}

type devicesModel struct {
	list list.Model
}

func newDevicesModel() devicesModel {
	l := newList()
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Host devices"
	l.SetShowHelp(false)
	return devicesModel{list: l}
}

func (m model) updateDevices(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case hostDevicesLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing host devices: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.devices))
		for i, d := range msg.devices {
			items[i] = deviceItem{dev: d}
		}
		m.devices.list.SetItems(items)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.sidebarFocused = true
			return m, nil
		case "r":
			return m, loadHostDevices(m.client)
		}
		var cmd tea.Cmd
		m.devices.list, cmd = m.devices.list.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m devicesModel) View() string {
	return m.list.View() + "\n" + helpBar("r", "refresh", "esc", "back")
}
