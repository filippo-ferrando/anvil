package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/list"

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
