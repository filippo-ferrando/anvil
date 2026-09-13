package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// downloadDedup collapses concurrent requests for the same destination
// path into one in-flight download — a minimal stand-in for
// golang.org/x/sync/singleflight (not fetchable in this environment
// without network access to `go get` it; swap Downloader.dedup for the
// real thing once that dependency can be added, no call-site changes
// needed since the behavior is identical).
type downloadDedup struct {
	mu       sync.Mutex
	inFlight map[string]*sync.WaitGroup
	results  map[string]error
}

func newDownloadDedup() *downloadDedup {
	return &downloadDedup{
		inFlight: make(map[string]*sync.WaitGroup),
		results:  make(map[string]error),
	}
}

// do runs fn for key, or waits for and returns the result of an
// already-in-flight call for the same key.
func (d *downloadDedup) do(key string, fn func() error) error {
	d.mu.Lock()
	if wg, ok := d.inFlight[key]; ok {
		d.mu.Unlock()
		wg.Wait()
		d.mu.Lock()
		err := d.results[key]
		d.mu.Unlock()
		return err
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	d.inFlight[key] = wg
	d.mu.Unlock()

	err := fn()

	d.mu.Lock()
	d.results[key] = err
	delete(d.inFlight, key)
	d.mu.Unlock()
	wg.Done()
	return err
}

// Downloader fetches a DistroEntry's image to a local path, verifying its
// SHA256 when the catalog entry specifies one.
type Downloader struct {
	dedup      *downloadDedup
	HTTPClient *http.Client
}

func NewDownloader() *Downloader {
	return &Downloader{dedup: newDownloadDedup(), HTTPClient: http.DefaultClient}
}

// Fetch downloads entry's image to destPath if it isn't already present
// with a matching checksum. Concurrent Fetch calls for the same destPath
// are deduplicated to a single download.
func (d *Downloader) Fetch(entry DistroEntry, destPath string) error {
	return d.dedup.do(destPath, func() error {
		if ok, _ := verifyExisting(destPath, entry.SHA256); ok {
			return nil
		}
		return d.download(entry, destPath)
	})
}

func verifyExisting(path, wantSHA256 string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if wantSHA256 == "" {
		// No checksum published for this entry — an existing file is
		// trusted as-is rather than re-downloaded every time. Real
		// per-distro checksums should be filled into the catalog
		// manifest before this is relied on for integrity, not just
		// idempotency (see data/distros/distribution-info.json's
		// current TODO state).
		return true, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA256, nil
}

func (d *Downloader) download(entry DistroEntry, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("image: creating cache dir: %w", err)
	}

	tmpPath := destPath + ".part"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("image: creating temp file: %w", err)
	}
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	resp, err := d.HTTPClient.Get(entry.URL)
	if err != nil {
		out.Close()
		return fmt.Errorf("image: fetching %s: %w", entry.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out.Close()
		return fmt.Errorf("image: fetching %s: unexpected status %s", entry.URL, resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), resp.Body); err != nil {
		out.Close()
		return fmt.Errorf("image: downloading %s: %w", entry.URL, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("image: closing %s: %w", tmpPath, err)
	}

	if entry.SHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != entry.SHA256 {
			return fmt.Errorf("image: checksum mismatch for %s: got %s, want %s", entry.ID, got, entry.SHA256)
		}
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("image: finalizing download: %w", err)
	}
	return nil
}
