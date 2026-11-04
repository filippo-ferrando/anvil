package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/data/distros"
)

// DefaultCatalogURL serves the official catalog's manifests, one file per name.
const DefaultCatalogURL = "https://git.fferrando.cc/anvil/catalog/raw/branch/main"

// DefaultCatalogInterval is how often a running daemon checks the official catalog.
const DefaultCatalogInterval = 24 * time.Hour

// Where the catalog in use came from.
const (
	CatalogEmbedded = "embedded"
	CatalogUpdated  = "updated"
)

// CatalogStatus describes the catalog in use and the last update check.
type CatalogStatus struct {
	AutoUpdate bool
	URL        string
	Interval   time.Duration
	Source     string    // CatalogEmbedded or CatalogUpdated
	UpdatedAt  time.Time // when the updated catalog in use was fetched, zero if embedded
	LastCheck  time.Time
	LastError  string
}

// CatalogUpdater replaces the embedded catalog with the official one's latest version,
// cached on disk so a restart without network keeps it.
type CatalogUpdater struct {
	URL       string // files are fetched from URL + "/" + name
	CachePath string
	Interval  time.Duration // 0 means checks only run on demand
	// FirstCheck is the delay between Run starting and its first check.
	FirstCheck time.Duration
	// Apply receives every new catalog after Check found a change.
	Apply  func(*Catalog)
	Client *http.Client

	embedded    []distros.File
	embeddedSum string

	checkMu sync.Mutex // one check at a time
	mu      sync.Mutex // guards current and status
	current []distros.File
	status  CatalogStatus
}

// catalogCache is the on-disk copy of the last fetched catalog.
type catalogCache struct {
	URL       string    `json:"url"`
	FetchedAt time.Time `json:"fetched_at"`
	// Embedded is the checksum of the embedded catalog at fetch time: a different
	// one means anvild was upgraded since, and its own catalog may be newer.
	Embedded string       `json:"embedded_sha256"`
	Files    []cachedFile `json:"files"`
}

type cachedFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// NewCatalogUpdater returns an updater over the embedded catalog, fetching from url.
func NewCatalogUpdater(url, cachePath string) (*CatalogUpdater, error) {
	files, err := distros.Files()
	if err != nil {
		return nil, fmt.Errorf("image: reading embedded catalog: %w", err)
	}
	return newCatalogUpdater(url, cachePath, files), nil
}

func newCatalogUpdater(url, cachePath string, embedded []distros.File) *CatalogUpdater {
	return &CatalogUpdater{
		URL:         strings.TrimRight(url, "/"),
		CachePath:   cachePath,
		Interval:    DefaultCatalogInterval,
		FirstCheck:  time.Minute,
		Client:      &http.Client{Timeout: manifestFetchTimeout},
		embedded:    embedded,
		embeddedSum: filesSum(embedded),
		current:     embedded,
		status:      CatalogStatus{Source: CatalogEmbedded},
	}
}

// Initial returns the catalog to start with: the cached update if it still fits this
// build and URL, the embedded catalog otherwise.
func (u *CatalogUpdater) Initial() (*Catalog, error) {
	if c, files, at, ok := u.loadCache(); ok {
		u.mu.Lock()
		u.current = files
		u.status.Source, u.status.UpdatedAt = CatalogUpdated, at
		u.mu.Unlock()
		return c, nil
	}
	return loadFiles(u.embedded)
}

func (u *CatalogUpdater) loadCache() (*Catalog, []distros.File, time.Time, bool) {
	raw, err := os.ReadFile(u.CachePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("image: reading catalog cache: %v", err)
		}
		return nil, nil, time.Time{}, false
	}
	var cache catalogCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		log.Printf("image: ignoring catalog cache %s: %v", u.CachePath, err)
		return nil, nil, time.Time{}, false
	}
	if cache.URL != u.URL || cache.Embedded != u.embeddedSum || len(cache.Files) == 0 {
		return nil, nil, time.Time{}, false
	}
	files := make([]distros.File, len(cache.Files))
	for i, f := range cache.Files {
		files[i] = distros.File{Name: f.Name, Raw: []byte(f.Content)}
	}
	c, err := loadFiles(files)
	if err != nil {
		log.Printf("image: ignoring catalog cache %s: %v", u.CachePath, err)
		return nil, nil, time.Time{}, false
	}
	return c, files, cache.FetchedAt, true
}

// Status reports the catalog in use and the last check.
func (u *CatalogUpdater) Status() CatalogStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.URL, s.Interval = u.URL, u.Interval
	s.AutoUpdate = u.Interval > 0
	return s
}

// Run checks the official catalog after FirstCheck, then every Interval, until ctx ends.
func (u *CatalogUpdater) Run(ctx context.Context) {
	if u.Interval <= 0 {
		return
	}
	wait := u.FirstCheck
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = u.Interval
		changed, err := u.Check(ctx)
		switch {
		case err != nil:
			// An offline host fails every check, so only log a new error.
			if err.Error() != lastErr {
				log.Printf("image: catalog update check: %v", err)
			}
			lastErr = err.Error()
		case changed:
			lastErr = ""
			log.Printf("image: catalog updated from %s", u.URL)
		default:
			lastErr = ""
		}
	}
}

// Check fetches the official catalog and applies it if it differs from the one in use.
func (u *CatalogUpdater) Check(ctx context.Context) (changed bool, err error) {
	u.checkMu.Lock()
	defer u.checkMu.Unlock()

	files, cat, err := u.fetch(ctx)
	now := time.Now()
	u.mu.Lock()
	u.status.LastCheck = now
	if err != nil {
		u.status.LastError = err.Error()
		u.mu.Unlock()
		return false, err
	}
	u.status.LastError = ""
	same := filesEqual(files, u.current)
	u.mu.Unlock()
	if same {
		return false, nil
	}

	// Back to the embedded catalog: no cache needed for it.
	upstreamIsEmbedded := filesEqual(files, u.embedded)
	if upstreamIsEmbedded {
		if err := os.Remove(u.CachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("image: removing catalog cache: %v", err)
		}
	} else if err := u.writeCache(files, now); err != nil {
		// Still use it, it just won't survive a restart.
		log.Printf("image: writing catalog cache: %v", err)
	}

	u.mu.Lock()
	u.current = files
	if upstreamIsEmbedded {
		u.status.Source, u.status.UpdatedAt = CatalogEmbedded, time.Time{}
	} else {
		u.status.Source, u.status.UpdatedAt = CatalogUpdated, now
	}
	u.mu.Unlock()
	if u.Apply != nil {
		u.Apply(cat)
	}
	return true, nil
}

// fetch downloads every manifest the embedded catalog has. A missing file means the
// catalog dropped it; a manifest that doesn't parse rejects the whole update.
func (u *CatalogUpdater) fetch(ctx context.Context) ([]distros.File, *Catalog, error) {
	var files []distros.File
	for _, e := range u.embedded {
		raw, err := u.fetchFile(ctx, u.URL+"/"+e.Name)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		files = append(files, distros.File{Name: e.Name, Raw: raw})
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("image: no catalog manifests found at %s", u.URL)
	}
	cat, err := loadFiles(files)
	if err != nil {
		return nil, nil, fmt.Errorf("image: catalog at %s is invalid: %w", u.URL, err)
	}
	return files, cat, nil
}

var errNotFound = errors.New("not found")

func (u *CatalogUpdater) fetchFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("image: building request for %s: %w", url, err)
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image: fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image: fetching %s: unexpected status %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("image: reading %s: %w", url, err)
	}
	if len(raw) > maxManifestBytes {
		return nil, fmt.Errorf("image: %s exceeds %d bytes", url, maxManifestBytes)
	}
	return raw, nil
}

func (u *CatalogUpdater) writeCache(files []distros.File, at time.Time) error {
	cache := catalogCache{URL: u.URL, FetchedAt: at, Embedded: u.embeddedSum}
	for _, f := range files {
		cache.Files = append(cache.Files, cachedFile{Name: f.Name, Content: string(f.Raw)})
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	dir := filepath.Dir(u.CachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".catalog-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), u.CachePath)
}

func filesEqual(a, b []distros.File) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !bytes.Equal(a[i].Raw, b[i].Raw) {
			return false
		}
	}
	return true
}

func filesSum(files []distros.File) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00", f.Name, len(f.Raw))
		h.Write(f.Raw)
	}
	return hex.EncodeToString(h.Sum(nil))
}
