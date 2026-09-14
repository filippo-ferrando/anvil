// Package cloudinitrepo fetches a cloud-init template repo's manifest and
// the templates it lists.
package cloudinitrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CurrentSchemaVersion is the manifest schema this build understands.
const CurrentSchemaVersion = 1

// fetchTimeout and maxFetchBytes bound a manifest or template fetch.
const fetchTimeout = 30 * time.Second
const maxFetchBytes = 10 << 20 // 10MiB

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
func FetchManifest(ctx context.Context, url string) (Manifest, error) {
	raw, err := fetch(ctx, url)
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

// FetchTemplate downloads one template's raw cloud-init content.
func FetchTemplate(ctx context.Context, url string) (string, error) {
	raw, err := fetch(ctx, url)
	if err != nil {
		return "", fmt.Errorf("cloudinitrepo: fetching template %s: %w", url, err)
	}
	return string(raw), nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	client := http.Client{Timeout: fetchTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFetchBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxFetchBytes)
	}
	return raw, nil
}
