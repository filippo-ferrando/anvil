package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

type cachedImageItem struct {
	image  *anvilv1.CachedImage
	sha256 string // filled in once asked for with "h"
}

func (i cachedImageItem) FilterValue() string { return i.image.GetId() }
func (i cachedImageItem) Title() string       { return i.image.GetId() }
func (i cachedImageItem) Description() string {
	inUse := "not in use"
	if n := i.image.GetRefCount(); n > 0 {
		inUse = fmt.Sprintf("in use by %d instance(s)", n)
	}
	desc := fmt.Sprintf("%s  •  %s  •  %s", i.image.GetArch(), progress.Bytes(i.image.GetSizeBytes()), inUse)
	switch i.image.GetKind() {
	case "firecracker-raw":
		desc = "firecracker raw copy  •  " + desc
	case "firecracker-kernel":
		desc = "firecracker kernel  •  " + desc
	}
	if i.sha256 != "" {
		desc += "  •  sha256 " + i.sha256[:12] + "…"
	}
	return desc
}

type catalogItem struct{ entry *anvilv1.CatalogEntry }

func (i catalogItem) FilterValue() string { return i.entry.GetId() }
func (i catalogItem) Title() string       { return i.entry.GetId() }
func (i catalogItem) Description() string {
	desc := fmt.Sprintf("%s %s  •  %s  •  min disk %d GiB  •  user %s",
		i.entry.GetDistro(), i.entry.GetVersion(), i.entry.GetArch(), i.entry.GetMinDiskGib(), i.entry.GetDefaultUser())
	if i.entry.GetFirecracker() {
		desc += "  •  firecracker"
	}
	return desc
}

type containerImageItem struct{ image *anvilv1.ContainerImage }

func (i containerImageItem) FilterValue() string { return i.image.GetId() }
func (i containerImageItem) Title() string {
	if tags := i.image.GetRepoTags(); len(tags) > 0 {
		return tags[0]
	}
	return "<untagged> " + format.ShortImageID(i.image.GetId())
}
func (i containerImageItem) Description() string {
	inUse := "not in use"
	if n := i.image.GetRefCount(); n > 0 {
		inUse = fmt.Sprintf("in use by %d container(s)", n)
	}
	return fmt.Sprintf("%s  •  %s  •  %s", format.Engine(i.image.GetEngine()), progress.Bytes(i.image.GetSizeBytes()), inUse)
}

// imagesMode selects which of the Images screen's two separate windows is showing.
type imagesMode int

const (
	imagesModeVM imagesMode = iota
	imagesModeContainer
)

// imagesModel is the Images screen: a VM window (cached images plus the catalog of what can be downloaded)
// and a container window (images cached by the configured engine), switched between with v/c.
type imagesModel struct {
	mode imagesMode

	cached        list.Model
	catalog       list.Model
	focusCatalog  bool
	confirmDelete *anvilv1.CachedImage

	// The full catalog and the "/" filter over it, matched like `anvil find <term>`.
	catalogAll    []*anvilv1.CatalogEntry
	catalogFilter textinput.Model
	filtering     bool // the filter input has the keyboard

	containerImages        list.Model
	confirmDeleteContainer *anvilv1.ContainerImage

	panelHeight int
}

func newImagesModel() imagesModel {
	cached := newList()
	cached.Title = "Cached (downloaded)"
	cached.SetShowHelp(false)
	cached.SetFilteringEnabled(false)

	catalog := newList()
	catalog.Title = "Available to launch"
	catalog.SetShowHelp(false)
	catalog.SetFilteringEnabled(false)

	containerImages := newList()
	containerImages.Title = "Cached container images"
	containerImages.SetShowHelp(false)
	containerImages.SetFilteringEnabled(false)

	filter := textinput.New()
	filter.Prompt = "/"
	filter.Placeholder = "id, distro or version"

	return imagesModel{cached: cached, catalog: catalog, containerImages: containerImages, catalogFilter: filter}
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

	fullInner := width - 2*boxOverhead
	if fullInner < 20 {
		fullInner = 20
	}
	m.containerImages.SetSize(fullInner, m.panelHeight)
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
		m.images.catalogAll = msg.entries
		m.images.applyCatalogFilter()
		return m, nil

	case containerImagesLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing container images: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.images))
		for i, img := range msg.images {
			items[i] = containerImageItem{image: img}
		}
		m.images.containerImages.SetItems(items)
		return m, nil

	case imageChecksumMsg:
		if msg.err != nil {
			m.setStatus("checksum: "+msg.err.Error(), true)
			return m, nil
		}
		for i, it := range m.images.cached.Items() {
			if item, ok := it.(cachedImageItem); ok && isBaseImage(item.image) && item.image.GetId() == msg.id && item.image.GetArch() == msg.arch {
				item.sha256 = msg.sum
				m.images.cached.SetItem(i, item)
			}
		}
		m.setStatus(fmt.Sprintf("%s (%s) sha256 %s", msg.id, msg.arch, msg.sum), false)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus("image "+msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus("image "+msg.verb, false)
		}
		if m.images.mode == imagesModeContainer {
			return m, loadContainerImages(m.client)
		}
		return m, loadCachedImages(m.client)

	case tea.KeyMsg:
		return m.updateImagesKey(msg)
	}
	return m, nil
}

// applyCatalogFilter shows the catalog entries matching the filter term.
func (m *imagesModel) applyCatalogFilter() {
	term := strings.TrimSpace(m.catalogFilter.Value())
	var items []list.Item
	for _, e := range m.catalogAll {
		if client.MatchesFindTerm(e, term) {
			items = append(items, catalogItem{entry: e})
		}
	}
	m.catalog.SetItems(items)
	m.catalog.Title = "Available to launch"
	if term != "" {
		m.catalog.Title += fmt.Sprintf(" (%d match %q)", len(items), term)
	}
}

// updateCatalogFilter edits the filter: enter keeps it, esc clears it.
func (m *imagesModel) updateCatalogFilter(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.filtering = false
		m.catalogFilter.Blur()
		return nil
	case "esc":
		m.filtering = false
		m.catalogFilter.Blur()
		m.catalogFilter.SetValue("")
		m.applyCatalogFilter()
		return nil
	}
	var cmd tea.Cmd
	m.catalogFilter, cmd = m.catalogFilter.Update(msg)
	m.applyCatalogFilter()
	return cmd
}

func (m model) updateImagesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	im := &m.images
	if im.filtering {
		return m, im.updateCatalogFilter(msg)
	}

	if im.confirmDelete != nil {
		switch k := msg.String(); k {
		case "y", "enter", "f":
			img := im.confirmDelete
			im.confirmDelete = nil
			return m, deleteCachedImage(m.client, img.GetId(), img.GetArch(), img.GetKind(), k == "f")
		default:
			im.confirmDelete = nil
			return m, nil
		}
	}
	if im.confirmDeleteContainer != nil {
		switch k := msg.String(); k {
		case "y", "enter", "f":
			img := im.confirmDeleteContainer
			im.confirmDeleteContainer = nil
			return m, deleteContainerImage(m.client, img.GetId(), img.GetEngine(), k == "f")
		default:
			im.confirmDeleteContainer = nil
			return m, nil
		}
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "v":
		im.mode = imagesModeVM
		return m, nil
	case "c":
		im.mode = imagesModeContainer
		return m, loadContainerImages(m.client)
	case "/":
		if im.mode == imagesModeVM {
			im.focusCatalog, im.filtering = true, true
			return m, im.catalogFilter.Focus()
		}
		return m, nil
	case "tab":
		if im.mode == imagesModeVM {
			im.focusCatalog = !im.focusCatalog
		}
		return m, nil
	case "r":
		if im.mode == imagesModeContainer {
			return m, loadContainerImages(m.client)
		}
		return m, tea.Batch(loadCachedImages(m.client), loadCatalog(m.client))
	case "h":
		if im.mode == imagesModeVM && !im.focusCatalog {
			if item, ok := im.cached.SelectedItem().(cachedImageItem); ok {
				if !isBaseImage(item.image) {
					m.setStatus("checksum covers base images only", true)
					return m, nil
				}
				m.setStatus("hashing "+item.image.GetId()+"…", false)
				return m, loadImageChecksum(m.client, item.image.GetId(), item.image.GetArch())
			}
		}
		return m, nil
	case "x":
		switch {
		case im.mode == imagesModeContainer:
			if item, ok := im.containerImages.SelectedItem().(containerImageItem); ok {
				im.confirmDeleteContainer = item.image
			}
		case !im.focusCatalog:
			if item, ok := im.cached.SelectedItem().(cachedImageItem); ok {
				im.confirmDelete = item.image
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch {
	case im.mode == imagesModeContainer:
		im.containerImages, cmd = im.containerImages.Update(msg)
	case im.focusCatalog:
		im.catalog, cmd = im.catalog.Update(msg)
	default:
		im.cached, cmd = im.cached.Update(msg)
	}
	return m, cmd
}

func (m imagesModel) View() string {
	if m.confirmDelete != nil {
		note := "only the download, not any distro entry"
		if isBaseImage(m.confirmDelete) {
			note += "; its firecracker raw copy goes too"
		}
		return styleWarn.Render(fmt.Sprintf("Delete cached image %q? (%s)", m.confirmDelete.GetId(), note)) +
			"\n\n" + helpBar("y", "confirm", "f", "force, even if an instance's disk uses it", "any other key", "cancel")
	}
	if m.confirmDeleteContainer != nil {
		label := m.confirmDeleteContainer.GetId()
		if tags := m.confirmDeleteContainer.GetRepoTags(); len(tags) > 0 {
			label = tags[0]
		}
		return styleWarn.Render(fmt.Sprintf("Delete container image %q?", label)) +
			"\n\n" + helpBar("y", "confirm", "f", "force, even if a container uses it", "any other key", "cancel")
	}

	tabs := imagesModeTabs(m.mode)

	if m.mode == imagesModeContainer {
		box := styleBoxFocused.Render(lipgloss.NewStyle().Height(m.panelHeight).Render(m.containerImages.View()))
		help := helpBar("v", "VM images", "c", "container images", "x", "delete", "r", "refresh", "esc", "back")
		return tabs + "\n" + box + "\n" + help
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

	help := helpBar("v", "VM images", "c", "container images", "tab", "switch panel", "/", "filter catalog",
		"h", "checksum", "x", "delete cached", "r", "refresh", "esc", "back")
	if m.filtering {
		help = m.catalogFilter.View() + "  " + helpBar("enter", "keep filter", "esc", "clear filter")
	}
	return tabs + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}

// imagesModeTabs renders the VM/Container mode switcher, highlighting whichever is active.
func imagesModeTabs(mode imagesMode) string {
	vmTab, containerTab := "VM Images", "Container Images"
	if mode == imagesModeVM {
		return styleMenuItemSelected.Render(vmTab) + " " + styleMenuItem.Render(containerTab)
	}
	return styleMenuItem.Render(vmTab) + " " + styleMenuItemSelected.Render(containerTab)
}

// isBaseImage reports whether img is a downloaded qcow2 base, not a raw copy or kernel.
func isBaseImage(img *anvilv1.CachedImage) bool {
	return img.GetKind() == "base"
}
