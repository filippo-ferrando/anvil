package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestChecksumOnlyForBaseImages(t *testing.T) {
	m := model{screen: screenImages, images: newImagesModel()}
	m.images.cached.SetItems([]list.Item{cachedImageItem{image: &anvilv1.CachedImage{Id: "debian:12", Kind: "firecracker-raw"}}})
	if _, cmd := m.Update(key("h")); cmd != nil {
		t.Error("expected no checksum request for a raw copy")
	}
	m.images.cached.SetItems([]list.Item{cachedImageItem{image: &anvilv1.CachedImage{Id: "debian:12", Kind: "base"}}})
	if _, cmd := m.Update(key("h")); cmd == nil {
		t.Error("expected a checksum request for a base image")
	}
}

func TestImageDeleteOffersForce(t *testing.T) {
	m := model{screen: screenImages, images: newImagesModel()}
	m.images.confirmDelete = &anvilv1.CachedImage{Id: "debian:12", Kind: "base"}
	if !strings.Contains(m.images.View(), "force") {
		t.Error("expected the confirmation to offer a forced delete")
	}
	next, cmd := m.Update(key("f"))
	if cmd == nil || next.(model).images.confirmDelete != nil {
		t.Error("expected f to delete and close the confirmation")
	}

	m.images.confirmDelete = nil
	m.images.confirmDeleteContainer = &anvilv1.ContainerImage{Id: "sha256:abc"}
	if !strings.Contains(m.images.View(), "force") {
		t.Error("expected the container confirmation to offer a forced delete")
	}
}

func TestCatalogFilter(t *testing.T) {
	m := model{screen: screenImages, images: newImagesModel()}
	next, _ := m.Update(catalogLoadedMsg{entries: []*anvilv1.CatalogEntry{
		{Id: "ubuntu-24.04", Distro: "ubuntu", Version: "24.04"},
		{Id: "debian-12", Distro: "debian", Version: "12"},
	}})
	m = next.(model)
	next, _ = m.Update(key("/"))
	m = next.(model)
	if !m.images.filtering || !m.images.focusCatalog {
		t.Fatal("expected / to focus the catalog filter")
	}
	for _, r := range "DEB" {
		next, _ = m.Update(key(string(r)))
		m = next.(model)
	}
	if items := m.images.catalog.Items(); len(items) != 1 || items[0].(catalogItem).entry.GetId() != "debian-12" {
		t.Fatalf("expected only debian-12, got %d items", len(items))
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.images.filtering || len(m.images.catalog.Items()) != 1 {
		t.Error("expected enter to keep the filter and leave the input")
	}
	// A reload keeps the filter applied.
	next, _ = m.Update(catalogLoadedMsg{entries: m.images.catalogAll})
	m = next.(model)
	if len(m.images.catalog.Items()) != 1 {
		t.Error("expected the filter to survive a reload")
	}
	m.images.filtering = true
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(next.(model).images.catalog.Items()) != 2 {
		t.Error("expected esc to clear the filter")
	}
}
