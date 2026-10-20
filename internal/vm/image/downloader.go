package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
)

// downloadDedup collapses concurrent requests for the same destination
// path into one in-flight download.
type downloadDedup struct {
	mu       sync.Mutex
	inFlight map[string]*flight
}

// flight is one shared download. It runs until it ends or every waiter gave up.
type flight struct {
	done     chan struct{}
	err      error
	cancel   context.CancelFunc
	canceled bool // every waiter left; set under downloadDedup.mu

	mu      sync.Mutex
	waiters map[*int]func(status string) // each waiter's progress, keyed by a unique pointer
}

func newDownloadDedup() *downloadDedup {
	return &downloadDedup{inFlight: make(map[string]*flight)}
}

// progress forwards status to every waiter still there.
func (f *flight) progress(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.waiters {
		if p != nil {
			p(status)
		}
	}
}

// do runs fn for key, or joins an already-in-flight call for the same key. fn's
// context ends only once every caller's ctx did, so one caller leaving doesn't fail the rest.
func (d *downloadDedup) do(ctx context.Context, key string, progress func(string), fn func(ctx context.Context, progress func(string)) error) error {
	d.mu.Lock()
	f, ok := d.inFlight[key]
	for ok && f.canceled {
		// Its last waiter left; let it wind down before starting again.
		d.mu.Unlock()
		<-f.done
		d.mu.Lock()
		f, ok = d.inFlight[key]
	}
	var fctx context.Context
	if !ok {
		var cancel context.CancelFunc
		fctx, cancel = context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{done: make(chan struct{}), cancel: cancel, waiters: map[*int]func(string){}}
		d.inFlight[key] = f
	}
	me := new(int)
	f.mu.Lock()
	f.waiters[me] = progress
	f.mu.Unlock()
	d.mu.Unlock()
	if fctx != nil {
		go func() {
			f.err = fn(fctx, f.progress)
			f.cancel()
			d.mu.Lock()
			delete(d.inFlight, key)
			d.mu.Unlock()
			close(f.done)
		}()
	}

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		d.mu.Lock()
		f.mu.Lock()
		delete(f.waiters, me)
		last := len(f.waiters) == 0
		f.mu.Unlock()
		if last {
			f.canceled = true
			f.cancel()
		}
		d.mu.Unlock()
		return ctx.Err()
	}
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

// Fetch downloads entry's image to destPath if not already present with
// a matching checksum, deduplicating concurrent calls for the same path.
func (d *Downloader) Fetch(ctx context.Context, entry DistroEntry, destPath string, progress func(status string)) error {
	return d.dedup.do(ctx, destPath, progress, func(ctx context.Context, progress func(string)) error {
		if ok, _ := verifyExisting(destPath, entry.SHA256); ok {
			return nil
		}
		return d.download(ctx, entry, destPath, progress)
	})
}

func verifyExisting(path, wantSHA256 string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if wantSHA256 == "" {
		// No checksum published: trust an existing file as-is.
		return true, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA256, nil
}

// downloadAttempts is how many times one Fetch resumes a transfer that broke midway.
const downloadAttempts = 4

// download fetches entry into destPath+".part", resuming a leftover partial file
// with an HTTP Range request, and renames it into place once the checksum matches.
func (d *Downloader) download(ctx context.Context, entry DistroEntry, destPath string, progress func(status string)) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("image: creating cache dir: %w", err)
	}
	tmpPath := destPath + ".part"
	metaPath := tmpPath + ".meta"

	if progress != nil {
		progress(fmt.Sprintf("downloading %s image from %s", entry.ID, entry.URL))
	}
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		var retry bool
		retry, err = d.fetchPart(ctx, entry, tmpPath, metaPath, progress)
		if err == nil || !retry || ctx.Err() != nil {
			break
		}
		if progress != nil && attempt < downloadAttempts {
			progress(fmt.Sprintf("download of %s interrupted, resuming (%v)", entry.ID, err))
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	if err != nil {
		return err
	}

	if progress != nil {
		progress(fmt.Sprintf("verifying checksum for %s", entry.ID))
	}
	got, err := fileSHA256(tmpPath)
	if err != nil {
		return fmt.Errorf("image: hashing %s: %w", tmpPath, err)
	}
	if entry.SHA256 != "" {
		if got != entry.SHA256 {
			// A corrupt partial file must not be resumed again.
			_ = os.Remove(tmpPath)
			_ = os.Remove(metaPath)
			return fmt.Errorf("image: checksum mismatch for %s: got %s, want %s", entry.ID, got, entry.SHA256)
		}
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("image: finalizing download: %w", err)
	}
	_ = os.Remove(metaPath)
	writeChecksumSidecar(destPath, got)
	return nil
}

// fetchPart appends the rest of entry's image to tmpPath. retry reports whether
// the error came from a broken transfer that a new Range request may finish.
func (d *Downloader) fetchPart(ctx context.Context, entry DistroEntry, tmpPath, metaPath string, progress func(status string)) (retry bool, err error) {
	var offset int64
	if fi, err := os.Stat(tmpPath); err == nil {
		offset = fi.Size()
	}
	validator, _ := os.ReadFile(metaPath)

	// No overall time limit or size cap: this is a multi-GB image that can
	// legitimately take a while; ctx cancellation still stops it.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.URL, nil)
	if err != nil {
		return false, fmt.Errorf("image: building request for %s: %w", entry.URL, err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		// If-Range makes the server send the whole file again if it changed since the partial download.
		if len(validator) > 0 {
			req.Header.Set("If-Range", string(validator))
		}
	}
	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("image: fetching %s: %w", entry.URL, err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	total := resp.ContentLength
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		flags |= os.O_APPEND
		if total >= 0 {
			total += offset
		}
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC
		offset = 0
		if v := resp.Header.Get("ETag"); v != "" && !strings.HasPrefix(v, "W/") {
			_ = os.WriteFile(metaPath, []byte(v), 0o640)
		} else if v := resp.Header.Get("Last-Modified"); v != "" {
			_ = os.WriteFile(metaPath, []byte(v), 0o640)
		} else {
			_ = os.Remove(metaPath)
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		// The partial file is already complete (or bigger than the image): start over once.
		_ = os.Remove(tmpPath)
		return true, fmt.Errorf("image: fetching %s: range not satisfiable", entry.URL)
	default:
		retry = resp.StatusCode >= 500
		return retry, fmt.Errorf("image: fetching %s: unexpected status %s", entry.URL, resp.Status)
	}

	out, err := os.OpenFile(tmpPath, flags, 0o640)
	if err != nil {
		return false, fmt.Errorf("image: opening temp file: %w", err)
	}
	body := io.Reader(resp.Body)
	if progress != nil {
		body = &progressReader{r: resp.Body, total: total, read: offset, label: "downloading " + entry.ID, progress: progress}
	}
	_, copyErr := io.Copy(out, body)
	if err := out.Close(); err != nil && copyErr == nil {
		return false, fmt.Errorf("image: closing %s: %w", tmpPath, err)
	}
	if copyErr != nil {
		return true, fmt.Errorf("image: downloading %s: %w", entry.URL, copyErr)
	}
	return false, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// progressReader wraps a download body, reporting byte progress through
// to progress at most every progressInterval.
type progressReader struct {
	r        io.Reader
	total    int64 // 0 if the server didn't send Content-Length
	read     int64
	lastSent time.Time
	label    string
	progress func(status string)
}

const progressInterval = 500 * time.Millisecond

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if time.Since(p.lastSent) >= progressInterval || err == io.EOF {
		p.progress(p.status())
		p.lastSent = time.Now()
	}
	return n, err
}

func (p *progressReader) status() string {
	if p.total > 0 {
		pct := float64(p.read) / float64(p.total) * 100
		return fmt.Sprintf("%s: %.0f%% (%s / %s)", p.label, pct, progress.Bytes(p.read), progress.Bytes(p.total))
	}
	return fmt.Sprintf("%s: %s", p.label, progress.Bytes(p.read))
}
