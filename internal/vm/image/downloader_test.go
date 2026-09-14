package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
