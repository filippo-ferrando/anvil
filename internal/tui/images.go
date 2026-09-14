package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

type cachedImageItem struct{ image *anvilv1.CachedImage }

func (i cachedImageItem) FilterValue() string { return i.image.GetId() }
func (i cachedImageItem) Title() string       { return i.image.GetId() }
func (i cachedImageItem) Description() string {
	inUse := "not in use"
	if n := i.image.GetRefCount(); n > 0 {
		inUse = fmt.Sprintf("in use by %d instance(s)", n)
	}
	return fmt.Sprintf("%s  •  %s  •  %s", i.image.GetArch(), humanBytesTUI(i.image.GetSizeBytes()), inUse)
}

type catalogItem struct{ entry *anvilv1.CatalogEntry }

func (i catalogItem) FilterValue() string { return i.entry.GetId() }
func (i catalogItem) Title() string       { return i.entry.GetId() }
func (i catalogItem) Description() string {
	return fmt.Sprintf("%s %s  •  %s  •  min disk %d GiB  •  user %s",
		i.entry.GetDistro(), i.entry.GetVersion(), i.entry.GetArch(), i.entry.GetMinDiskGib(), i.entry.GetDefaultUser())
}

// imagesModel is the Images screen: cached base images alongside the catalog of what can be downloaded.
type imagesModel struct {
	cached        list.Model
	catalog       list.Model
	focusCatalog  bool
	confirmDelete *anvilv1.CachedImage
	panelHeight   int
}

func newImagesModel() imagesModel {
	cached := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	cached.Title = "Cached (downloaded)"
	cached.SetShowHelp(false)
	cached.SetFilteringEnabled(false)

	catalog := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	catalog.Title = "Available to launch"
	catalog.SetShowHelp(false)
	catalog.SetFilteringEnabled(false)

	return imagesModel{cached: cached, catalog: catalog}
}

func (m *imagesModel) setSize(width, height int) {
	const gutter = 2
	inner := width - 2*boxOverhead - gutter
	if inner < 20 {
		inner = 20
	}
	half := inner / 2
	m.panelHeight = height - boxHeightOverhead
	if m.panelHeight < 3 {
		m.panelHeight = 3
	}
	m.cached.SetSize(half, m.panelHeight)
	m.catalog.SetSize(inner-half, m.panelHeight)
}

func (m model) updateImages(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case cachedImagesLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing cached images: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.images))
		for i, img := range msg.images {
			items[i] = cachedImageItem{image: img}
		}
		m.images.cached.SetItems(items)
		return m, nil

	case catalogLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing the catalog: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.entries))
		for i, e := range msg.entries {
			items[i] = catalogItem{entry: e}
		}
		m.images.catalog.SetItems(items)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus("image "+msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus("image "+msg.verb, false)
		}
		return m, loadCachedImages(m.client)

	case tea.KeyMsg:
		return m.updateImagesKey(msg)
	}
	return m, nil
}

func (m model) updateImagesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	im := &m.images

	if im.confirmDelete != nil {
		switch msg.String() {
		case "y", "enter":
			id := im.confirmDelete.GetId()
			im.confirmDelete = nil
			return m, deleteCachedImage(m.client, id)
		default:
			im.confirmDelete = nil
			return m, nil
		}
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "tab":
		im.focusCatalog = !im.focusCatalog
		return m, nil
	case "r":
		return m, tea.Batch(loadCachedImages(m.client), loadCatalog(m.client))
	case "x":
		if !im.focusCatalog {
			if item, ok := im.cached.SelectedItem().(cachedImageItem); ok {
				im.confirmDelete = item.image
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	if im.focusCatalog {
		im.catalog, cmd = im.catalog.Update(msg)
	} else {
		im.cached, cmd = im.cached.Update(msg)
	}
	return m, cmd
}

func (m imagesModel) View() string {
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Delete cached image %q? (only the download, not any distro entry)", m.confirmDelete.GetId())) +
			"\n\n" + helpBar("y", "confirm", "any other key", "cancel")
	}

	cachedBox, catalogBox := styleBox, styleBox
	if m.focusCatalog {
		catalogBox = styleBoxFocused
	} else {
		cachedBox = styleBoxFocused
	}
	pin := lipgloss.NewStyle().Height(m.panelHeight)
	left := cachedBox.Render(pin.Render(m.cached.View()))
	right := catalogBox.Render(pin.Render(m.catalog.View()))

	help := helpBar("tab", "switch panel", "x", "delete cached", "r", "refresh", "esc", "back")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}
