package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDownloaderFetchReportsProgress(t *testing.T) {
	payload := strings.Repeat("x", 4096)
	sum := sha256.Sum256([]byte(payload))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "fake.qcow2")
	entry := DistroEntry{ID: "fake-distro", URL: srv.URL, SHA256: hex.EncodeToString(sum[:])}

	d := NewDownloader()
	var statuses []string
	err := d.Fetch(context.Background(), entry, dest, func(status string) { statuses = append(statuses, status) })
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(statuses) == 0 {
		t.Fatal("expected at least one progress update, got none")
	}
	if !strings.Contains(statuses[0], "downloading fake-distro") {
		t.Errorf("expected the first status to announce the download starting, got %q", statuses[0])
	}
	last := statuses[len(statuses)-1]
	if !strings.Contains(last, "verifying checksum") {
		t.Errorf("expected the final status to mention checksum verification, got %q", last)
	}
}

func TestDownloaderFetchSkippedWhenAlreadyCached(t *testing.T) {
	payload := "cached content"
	sum := sha256.Sum256([]byte(payload))

	dir := t.TempDir()
	dest := filepath.Join(dir, "fake.qcow2")
	if err := os.WriteFile(dest, []byte(payload), 0o644); err != nil {
		t.Fatalf("seeding cached file: %v", err)
	}

	entry := DistroEntry{ID: "fake-distro", URL: "https://example.invalid/should-not-be-fetched", SHA256: hex.EncodeToString(sum[:])}
	d := NewDownloader()
	called := false
	if err := d.Fetch(context.Background(), entry, dest, func(string) { called = true }); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if called {
		t.Error("expected no progress callback when the file is already cached with a matching checksum")
	}
}

// rangeServer serves payload, honoring Range requests, and cuts the first
// response off after cutAt bytes when cutAt > 0.
func rangeServer(t *testing.T, payload string, cutAt int, ranges *[]string) *httptest.Server {
	t.Helper()
	var calls int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		*ranges = append(*ranges, r.Header.Get("Range"))
		w.Header().Set("ETag", `"v1"`)
		if calls == 1 && cutAt > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write([]byte(payload[:cutAt]))
			// Hijack and close so the client sees an unexpected EOF.
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		http.ServeContent(w, r, "img", time.Time{}, strings.NewReader(payload))
	}))
}

func TestDownloaderResumesAfterBrokenTransfer(t *testing.T) {
	payload := strings.Repeat("abcdefgh", 1024)
	sum := sha256.Sum256([]byte(payload))
	var ranges []string
	srv := rangeServer(t, payload, 3000, &ranges)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "img.qcow2")
	entry := DistroEntry{ID: "fake", URL: srv.URL, SHA256: hex.EncodeToString(sum[:])}
	if err := NewDownloader().Fetch(context.Background(), entry, dest, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != payload {
		t.Fatalf("downloaded content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if len(ranges) != 2 || ranges[1] != "bytes=3000-" {
		t.Errorf("expected a second request resuming at byte 3000, got %q", ranges)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("expected the .part file to be gone after a finished download")
	}
}

func TestDownloaderResumesLeftoverPartFile(t *testing.T) {
	payload := strings.Repeat("0123456789", 500)
	sum := sha256.Sum256([]byte(payload))
	var ranges []string
	srv := rangeServer(t, payload, 0, &ranges)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "img.qcow2")
	if err := os.WriteFile(dest+".part", []byte(payload[:1234]), 0o640); err != nil {
		t.Fatal(err)
	}
	entry := DistroEntry{ID: "fake", URL: srv.URL, SHA256: hex.EncodeToString(sum[:])}
	if err := NewDownloader().Fetch(context.Background(), entry, dest, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != payload {
		t.Fatal("downloaded content mismatch")
	}
	if len(ranges) != 1 || ranges[0] != "bytes=1234-" {
		t.Errorf("expected one request resuming at byte 1234, got %q", ranges)
	}
}

func TestDownloaderChecksumMismatchDropsPartFile(t *testing.T) {
	var ranges []string
	srv := rangeServer(t, "real content", 0, &ranges)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "img.qcow2")
	entry := DistroEntry{ID: "fake", URL: srv.URL, SHA256: strings.Repeat("0", 64)}
	if err := NewDownloader().Fetch(context.Background(), entry, dest, nil); err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("expected a corrupt .part file to be removed")
	}
}
