package daemon

import (
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMissingRecordsMapToNotFound checks that a lookup for something that
// isn't there reaches the client as codes.NotFound rather than an opaque
// error, across every server that reads the registry directly.
func TestMissingRecordsMapToNotFound(t *testing.T) {
	s := testStore(t)
	ci := NewCloudInitServer(s)
	mirrors := NewMirrorServer(s)
	hosts := NewHostServer(s, nil)

	tests := []struct {
		name string
		call func() error
	}{
		{"cloud-init get", func() error {
			_, err := ci.Get(t.Context(), &anvilv1.CloudInitGetRequest{Name: "nope"})
			return err
		}},
		{"cloud-init rename", func() error {
			_, err := ci.Rename(t.Context(), &anvilv1.CloudInitRenameRequest{OldName: "nope", NewName: "x"})
			return err
		}},
		{"cloud-init delete", func() error {
			_, err := ci.Delete(t.Context(), &anvilv1.CloudInitDeleteRequest{Name: "nope"})
			return err
		}},
		{"mirror remove", func() error {
			_, err := mirrors.Remove(t.Context(), &anvilv1.MirrorRemoveRequest{Name: "nope"})
			return err
		}},
		{"mirror set-enabled", func() error {
			_, err := mirrors.SetEnabled(t.Context(), &anvilv1.MirrorSetEnabledRequest{Name: "nope", Enabled: true})
			return err
		}},
		{"host remove", func() error {
			_, err := hosts.Remove(t.Context(), &anvilv1.HostRemoveRequest{Alias: "nope"})
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected an error for a record that doesn't exist")
			}
			if got := status.Code(err); got != codes.NotFound {
				t.Errorf("status code is %v, want %v (error: %v)", got, codes.NotFound, err)
			}
		})
	}
}

// TestCloudInitServerRoundTrip drives the save/list/get/rename/delete path the
// CLI and TUI both use.
func TestCloudInitServerRoundTrip(t *testing.T) {
	ci := NewCloudInitServer(testStore(t))
	const body = "#cloud-config\npackages: [git]\n"

	if _, err := ci.Save(t.Context(), &anvilv1.CloudInitSaveRequest{Name: "base", Content: body}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	list, err := ci.List(t.Context(), &anvilv1.CloudInitListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.GetConfigs()) != 1 || list.GetConfigs()[0].GetName() != "base" {
		t.Fatalf("List returned %+v", list.GetConfigs())
	}

	got, err := ci.Get(t.Context(), &anvilv1.CloudInitGetRequest{Name: "base"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetContent() != body {
		t.Errorf("content is %q, want %q", got.GetContent(), body)
	}

	if _, err := ci.Rename(t.Context(), &anvilv1.CloudInitRenameRequest{OldName: "base", NewName: "renamed"}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := ci.Get(t.Context(), &anvilv1.CloudInitGetRequest{Name: "base"}); status.Code(err) != codes.NotFound {
		t.Errorf("the old name should be gone, got %v", err)
	}
	if _, err := ci.Delete(t.Context(), &anvilv1.CloudInitDeleteRequest{Name: "renamed"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestMirrorServerRoundTrip covers add, list filtering, enable/disable and
// remove without touching the network: a VM mirror with inline JSON never
// fetches its manifest.
func TestMirrorServerRoundTrip(t *testing.T) {
	m := NewMirrorServer(testStore(t))

	_, err := m.Add(t.Context(), &anvilv1.MirrorAddRequest{Mirror: &anvilv1.Mirror{
		Name: "corp", Kind: anvilv1.MirrorKind_MIRROR_KIND_CONTAINER,
		Registry: "registry.corp:5000", MirrorOf: "docker.io",
	}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	list, err := m.List(t.Context(), &anvilv1.MirrorListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.GetMirrors()) != 1 {
		t.Fatalf("List returned %d mirrors, want 1", len(list.GetMirrors()))
	}
	if !list.GetMirrors()[0].GetEnabled() {
		t.Error("a newly added mirror should be enabled")
	}

	if _, err := m.SetEnabled(t.Context(), &anvilv1.MirrorSetEnabledRequest{Name: "corp", Enabled: false}); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	list, _ = m.List(t.Context(), &anvilv1.MirrorListRequest{})
	if list.GetMirrors()[0].GetEnabled() {
		t.Error("the mirror should be disabled after SetEnabled(false)")
	}

	if _, err := m.Remove(t.Context(), &anvilv1.MirrorRemoveRequest{Name: "corp"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list, _ = m.List(t.Context(), &anvilv1.MirrorListRequest{})
	if len(list.GetMirrors()) != 0 {
		t.Errorf("expected no mirrors left, got %d", len(list.GetMirrors()))
	}
}

func TestHostServerRoundTrip(t *testing.T) {
	h := NewHostServer(testStore(t), nil)

	if _, err := h.Add(t.Context(), &anvilv1.HostAddRequest{Host: &anvilv1.Host{
		Alias: "box", Target: "root@10.0.0.2", StrictHostKey: true,
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	list, err := h.List(t.Context(), &anvilv1.HostListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.GetHosts()) != 1 || list.GetHosts()[0].GetTarget() != "root@10.0.0.2" {
		t.Fatalf("List returned %+v", list.GetHosts())
	}
	if !list.GetHosts()[0].GetStrictHostKey() {
		t.Error("strict host key flag was not persisted")
	}
	if _, err := h.Remove(t.Context(), &anvilv1.HostRemoveRequest{Alias: "box"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestPortConversionRoundTrips(t *testing.T) {
	in := []instance.PortMapping{
		{HostPort: 8080, GuestPort: 80, Protocol: "tcp"},
		{HostPort: 5353, GuestPort: 53, Protocol: "udp"},
		{HostPort: 2222, GuestPort: 22}, // protocol left unset on purpose
	}
	got := portsFromPB(portsToPB(in))
	if len(got) != len(in) {
		t.Fatalf("round trip returned %d ports, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("port %d round-tripped to %+v, want %+v", i, got[i], in[i])
		}
	}
	if n := len(portsFromPB(nil)); n != 0 {
		t.Errorf("nil converted to %d ports, want 0", n)
	}
}
