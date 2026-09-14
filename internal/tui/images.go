package tui

import (
	"fmt"
	"strings"

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

type containerImageItem struct{ image *anvilv1.ContainerImage }

func (i containerImageItem) FilterValue() string { return i.image.GetId() }
func (i containerImageItem) Title() string {
	if tags := i.image.GetRepoTags(); len(tags) > 0 {
		return tags[0]
	}
	return "<untagged> " + shortImageID(i.image.GetId())
}
func (i containerImageItem) Description() string {
	inUse := "not in use"
	if n := i.image.GetRefCount(); n > 0 {
		inUse = fmt.Sprintf("in use by %d container(s)", n)
	}
	return fmt.Sprintf("%s  •  %s  •  %s", engineLabelTUI(i.image.GetEngine()), humanBytesTUI(i.image.GetSizeBytes()), inUse)
}

// shortImageID trims a "sha256:" prefix and shortens to 12 hex characters,
// matching `docker images`' own convention.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func engineLabelTUI(e anvilv1.ContainerEngine) string {
	if e == anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN {
		return "podman"
	}
	return "docker" // unset defaults to docker
}

// imagesMode selects which of the Images screen's two separate windows is showing.
type imagesMode int

const (
	imagesModeVM imagesMode = iota
	imagesModeContainer
)

// imagesModel is the Images screen: a VM window (cached base images
// alongside the catalog of what can be downloaded) and a separate
// container window (images cached by the configured container engine),
// switched between with v/c.
type imagesModel struct {
	mode imagesMode

	cached        list.Model
	catalog       list.Model
	focusCatalog  bool
	confirmDelete *anvilv1.CachedImage

	containerImages        list.Model
	confirmDeleteContainer *anvilv1.ContainerImage

	panelHeight int
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

	containerImages := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	containerImages.Title = "Cached container images"
	containerImages.SetShowHelp(false)
	containerImages.SetFilteringEnabled(false)

	return imagesModel{cached: cached, catalog: catalog, containerImages: containerImages}
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
		items := make([]list.Item, len(msg.entries))
		for i, e := range msg.entries {
			items[i] = catalogItem{entry: e}
		}
		m.images.catalog.SetItems(items)
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
	if im.confirmDeleteContainer != nil {
		switch msg.String() {
		case "y", "enter":
			img := im.confirmDeleteContainer
			im.confirmDeleteContainer = nil
			return m, deleteContainerImage(m.client, img.GetId(), img.GetEngine())
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
		return styleWarn.Render(fmt.Sprintf("Delete cached image %q? (only the download, not any distro entry)", m.confirmDelete.GetId())) +
			"\n\n" + helpBar("y", "confirm", "any other key", "cancel")
	}
	if m.confirmDeleteContainer != nil {
		label := m.confirmDeleteContainer.GetId()
		if tags := m.confirmDeleteContainer.GetRepoTags(); len(tags) > 0 {
			label = tags[0]
		}
		return styleWarn.Render(fmt.Sprintf("Delete container image %q?", label)) +
			"\n\n" + helpBar("y", "confirm", "any other key", "cancel")
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

	help := helpBar("v", "VM images", "c", "container images", "tab", "switch panel",
		"x", "delete cached", "r", "refresh", "esc", "back")
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
