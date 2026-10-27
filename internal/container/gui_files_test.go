package container

import (
	"archive/tar"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// Reading the files of a listed dir reuses the listing's archive instead of one call each.
func TestReadGuestFileUsesListing(t *testing.T) {
	var calls atomic.Int32
	dir, err := os.MkdirTemp("", "anvil-docker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("path") != "/usr/share/applications" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: "applications/", Typeflag: tar.TypeDir, Mode: 0o755})
		body := []byte("[Desktop Entry]\nName=Gimp\n")
		_ = tw.WriteHeader(&tar.Header{Name: "applications/gimp.desktop", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write(body)
		_ = tw.WriteHeader(&tar.Header{Name: "applications/alias.desktop", Typeflag: tar.TypeSymlink, Linkname: "gimp.desktop"})
		_ = tw.Close()
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	b := &DockerBackend{Client: docker.NewClient(sock)}
	spec := &instance.Spec{Name: "c", Container: &instance.ContainerSpec{ContainerID: "abc"}}
	names, err := b.ListGuestDir(t.Context(), spec, "/usr/share/applications")
	if err != nil || len(names) != 2 {
		t.Fatalf("ListGuestDir = %v, %v", names, err)
	}
	for _, p := range []string{"/usr/share/applications/gimp.desktop", "/usr/share/applications/alias.desktop"} {
		data, ok, err := b.ReadGuestFile(t.Context(), spec, p)
		if err != nil || !ok || string(data) != "[Desktop Entry]\nName=Gimp\n" {
			t.Errorf("ReadGuestFile(%s) = %q, %v, %v", p, data, ok, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("Docker called %d times, want 1", n)
	}
	// A file outside the listing still goes to Docker.
	if _, ok, _ := b.ReadGuestFile(t.Context(), spec, "/etc/other"); ok || calls.Load() != 2 {
		t.Errorf("expected a miss through Docker, ok=%v calls=%d", ok, calls.Load())
	}
}
