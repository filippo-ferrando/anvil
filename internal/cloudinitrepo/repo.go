// Package cloudinitrepo fetches a cloud-init template repo's manifest and
// the templates it lists — the cloud-init-library equivalent of
// internal/vm/image's VM mirror manifest handling, see docs/mirrors.md
// for the manifest shape and how to host one. Pure stdlib (net/http,
// encoding/json), same reasoning as internal/vm/image.FetchManifest: no
// dependency whose exact behavior needs verifying, just a GET and a JSON
// parse.
package cloudinitrepo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CurrentSchemaVersion is the manifest schema this build understands —
// same "reject an unrecognized major version outright" rule
// internal/vm/image.Catalog's own schema_version already uses.
const CurrentSchemaVersion = 1

// Template describes one cloud-init config a repo manifest lists.
type Template struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// Manifest is the on-disk (well, on-the-wire) shape of a template repo's
// manifest document.
type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Templates     []Template `json:"templates"`
}

// FetchManifest downloads and validates a template repo's manifest.
func FetchManifest(url string) (Manifest, error) {
	raw, err := fetch(url)
	if err != nil {
		return Manifest{}, fmt.Errorf("cloudinitrepo: fetching manifest %s: %w", url, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("cloudinitrepo: parsing manifest %s: %w", url, err)
	}
	if m.SchemaVersion != CurrentSchemaVersion {
		return Manifest{}, fmt.Errorf("cloudinitrepo: manifest %s has schema_version %d, this build expects %d",
			url, m.SchemaVersion, CurrentSchemaVersion)
	}
	for i, t := range m.Templates {
		if t.Name == "" {
			return Manifest{}, fmt.Errorf("cloudinitrepo: manifest %s: templates[%d] has no name", url, i)
		}
		if t.URL == "" {
			return Manifest{}, fmt.Errorf("cloudinitrepo: manifest %s: template %q has no url", url, t.Name)
		}
	}
	return m, nil
}

// FetchTemplate downloads one template's raw cloud-init content (the
// exact text that becomes a saved library entry's content, unmodified).
func FetchTemplate(url string) (string, error) {
	raw, err := fetch(url)
	if err != nil {
		return "", fmt.Errorf("cloudinitrepo: fetching template %s: %w", url, err)
	}
	return string(raw), nil
}

func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}
