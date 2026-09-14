package cloudinitrepo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchManifestValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"schema_version": 1,
			"templates": [
				{"name": "nginx", "description": "web server", "url": "https://example.com/nginx.yaml"},
				{"name": "postgres", "description": "database", "url": "https://example.com/postgres.yaml"}
			]
		}`)
	}))
	defer srv.Close()

	m, err := FetchManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if len(m.Templates) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(m.Templates))
	}
	if m.Templates[0].Name != "nginx" || m.Templates[1].Name != "postgres" {
		t.Fatalf("unexpected templates: %+v", m.Templates)
	}
}

func TestFetchManifestRejectsWrongSchemaVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"schema_version": 99, "templates": []}`)
	}))
	defer srv.Close()

	if _, err := FetchManifest(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for an unrecognized schema_version")
	}
}

func TestFetchManifestRejectsMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"schema_version": 1, "templates": [{"name": "", "url": "https://example.com/x.yaml"}]}`)
	}))
	defer srv.Close()

	if _, err := FetchManifest(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a template with no name")
	}
}

func TestFetchManifestRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := FetchManifest(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a 404")
	}
}

func TestFetchTemplate(t *testing.T) {
	const content = "#cloud-config\npackages:\n  - nginx\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, content)
	}))
	defer srv.Close()

	got, err := FetchTemplate(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchTemplate: %v", err)
	}
	if got != content {
		t.Fatalf("expected %q, got %q", content, got)
	}
}

func TestFetchTemplateRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := FetchTemplate(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a 500")
	}
	if _, err := FetchTemplate(context.Background(), "http://127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "cloudinitrepo") {
		t.Fatalf("expected a wrapped cloudinitrepo error for an unreachable host, got: %v", err)
	}
}
