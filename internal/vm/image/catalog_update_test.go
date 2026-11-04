package image

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/data/distros"
)

const (
	oldDistros = `{"schema_version": 1, "distros": [{"id": "debian-12", "arch": "x86_64", "url": "https://a.example/old.img"}]}`
	newDistros = `{"schema_version": 1, "distros": [{"id": "debian-12", "arch": "x86_64", "url": "https://a.example/new.img"},
		{"id": "debian-13", "arch": "x86_64", "url": "https://a.example/13.img"}],
		"firecracker": {"kernels": [{"arch": "x86_64", "version": "7.0", "url": "https://a.example/k"}], "distros": [{"id": "debian-13"}]}}`
)

// catalogServer serves files by name; a missing name is a 404.
type catalogServer struct {
	mu    sync.Mutex
	files map[string]string
	*httptest.Server
}

func newCatalogServer(t *testing.T, files map[string]string) *catalogServer {
	s := &catalogServer{files: files}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body, ok := s.files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *catalogServer) set(name, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = body
}

func testUpdater(t *testing.T, url string) *CatalogUpdater {
	embedded := []distros.File{{Name: "distros_x86_64.json", Raw: []byte(oldDistros)}}
	return newCatalogUpdater(url, filepath.Join(t.TempDir(), "catalog.json"), embedded)
}

func TestCatalogUpdateAppliesAndCaches(t *testing.T) {
	srv := newCatalogServer(t, map[string]string{"distros_x86_64.json": newDistros})
	u := testUpdater(t, srv.URL+"/")
	var applied *Catalog
	u.Apply = func(c *Catalog) { applied = c }

	changed, err := u.Check(context.Background())
	if err != nil || !changed {
		t.Fatalf("Check = %v, %v; want a change", changed, err)
	}
	if e, err := applied.Find("debian-12", "x86_64"); err != nil || e.URL != "https://a.example/new.img" {
		t.Errorf("Find = %+v, %v", e, err)
	}
	if k, err := applied.FirecrackerKernel("x86_64"); err != nil || k.Version != "7.0" {
		t.Errorf("FirecrackerKernel = %+v, %v", k, err)
	}
	if st := u.Status(); st.Source != CatalogUpdated || st.UpdatedAt.IsZero() || st.LastError != "" {
		t.Errorf("Status = %+v", st)
	}

	if changed, err := u.Check(context.Background()); err != nil || changed {
		t.Errorf("second Check = %v, %v; want no change", changed, err)
	}

	// A restart picks the cache up without network.
	again := newCatalogUpdater(u.URL, u.CachePath, u.embedded)
	cat, err := again.Initial()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Find("debian-13", "x86_64"); err != nil {
		t.Errorf("expected the cached catalog after a restart: %v", err)
	}
	if again.Status().Source != CatalogUpdated {
		t.Errorf("Source = %q", again.Status().Source)
	}
}

// A cache fetched by another build is ignored: that build's embedded catalog may be newer.
func TestCatalogCacheIgnoredAfterUpgrade(t *testing.T) {
	srv := newCatalogServer(t, map[string]string{"distros_x86_64.json": newDistros})
	u := testUpdater(t, srv.URL)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	upgraded := newCatalogUpdater(u.URL, u.CachePath, []distros.File{{Name: "distros_x86_64.json", Raw: []byte(newDistros + " ")}})
	cat, err := upgraded.Initial()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Find("debian-13", "x86_64"); err != nil {
		t.Errorf("expected the upgraded build's embedded catalog: %v", err)
	}
	if upgraded.Status().Source != CatalogEmbedded {
		t.Errorf("Source = %q", upgraded.Status().Source)
	}
}

func TestCatalogUpdateRejectsInvalid(t *testing.T) {
	srv := newCatalogServer(t, map[string]string{"distros_x86_64.json": `{"schema_version": 99, "distros": []}`})
	u := testUpdater(t, srv.URL)
	u.Apply = func(*Catalog) { t.Error("an invalid catalog must not be applied") }
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("expected an error for an unsupported schema")
	}
	if st := u.Status(); st.LastError == "" || st.LastCheck.IsZero() || st.Source != CatalogEmbedded {
		t.Errorf("Status = %+v", st)
	}
	if _, err := os.Stat(u.CachePath); !os.IsNotExist(err) {
		t.Errorf("expected no cache, stat = %v", err)
	}
}

func TestCatalogUpdateNothingFound(t *testing.T) {
	srv := newCatalogServer(t, map[string]string{})
	u := testUpdater(t, srv.URL)
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("expected an error when no manifest exists")
	}
}

// Upstream going back to what the build embeds drops the cache.
func TestCatalogUpdateBackToEmbedded(t *testing.T) {
	srv := newCatalogServer(t, map[string]string{"distros_x86_64.json": newDistros})
	u := testUpdater(t, srv.URL)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.set("distros_x86_64.json", oldDistros)
	if changed, err := u.Check(context.Background()); err != nil || !changed {
		t.Fatalf("Check = %v, %v; want a change", changed, err)
	}
	if st := u.Status(); st.Source != CatalogEmbedded || !st.UpdatedAt.IsZero() {
		t.Errorf("Status = %+v", st)
	}
	if _, err := os.Stat(u.CachePath); !os.IsNotExist(err) {
		t.Errorf("expected the cache to be removed, stat = %v", err)
	}
}

func TestCatalogRunDisabled(t *testing.T) {
	u := testUpdater(t, "http://127.0.0.1:0")
	u.Interval = 0
	u.Run(context.Background()) // returns right away instead of blocking
	if u.Status().AutoUpdate {
		t.Error("expected AutoUpdate off with no interval")
	}
}
