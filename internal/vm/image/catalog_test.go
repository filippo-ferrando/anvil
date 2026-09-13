package image

import "testing"

func TestLoadEmbeddedCatalog(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	entries := cat.List()
	if len(entries) == 0 {
		t.Fatal("expected at least one catalog entry")
	}

	entry, err := cat.Find("ubuntu-24.04", "x86_64")
	if err != nil {
		t.Fatalf("Find(ubuntu-24.04): %v", err)
	}
	if entry.URL == "" {
		t.Error("expected ubuntu-24.04 entry to have a URL")
	}
	if entry.MinDiskGiB <= 0 {
		t.Error("expected a positive min disk size")
	}
}

func TestFindDefaultsToX86_64(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	if _, err := cat.Find("ubuntu-24.04", ""); err != nil {
		t.Errorf("expected empty arch to default to x86_64: %v", err)
	}
}

func TestFindUnknownEntry(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	if _, err := cat.Find("does-not-exist", "x86_64"); err == nil {
		t.Error("expected an error for an unknown catalog id")
	}
}

func TestRejectsWrongSchemaVersion(t *testing.T) {
	_, err := loadManifest([]byte(`{"schema_version": 99, "distros": []}`), 0)
	if err == nil {
		t.Error("expected an error for an unsupported schema_version")
	}
}

func TestWithMirrorsOverridesOnHigherPriority(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	mirrorManifest := `{"schema_version": 1, "distros": [
		{"id": "ubuntu-24.04", "arch": "x86_64", "url": "https://mirror.example/ubuntu-24.04.img", "min_disk_gib": 3}
	]}`
	merged, err := cat.WithMirrors([]MirrorManifest{{ManifestJSON: mirrorManifest, Priority: 10}})
	if err != nil {
		t.Fatalf("WithMirrors: %v", err)
	}
	entry, err := merged.Find("ubuntu-24.04", "x86_64")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if entry.URL != "https://mirror.example/ubuntu-24.04.img" {
		t.Errorf("expected the higher-priority mirror entry to win, got URL %q", entry.URL)
	}

	// The original catalog must be untouched.
	original, err := cat.Find("ubuntu-24.04", "x86_64")
	if err != nil {
		t.Fatalf("Find on original: %v", err)
	}
	if original.URL == entry.URL {
		t.Error("expected WithMirrors to not mutate the receiver")
	}
}

func TestWithMirrorsLowerPriorityDoesNotOverride(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	original, err := cat.Find("ubuntu-24.04", "x86_64")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	mirrorManifest := `{"schema_version": 1, "distros": [
		{"id": "ubuntu-24.04", "arch": "x86_64", "url": "https://mirror.example/lower-priority.img", "min_disk_gib": 3}
	]}`
	merged, err := cat.WithMirrors([]MirrorManifest{{ManifestJSON: mirrorManifest, Priority: -10}})
	if err != nil {
		t.Fatalf("WithMirrors: %v", err)
	}
	entry, err := merged.Find("ubuntu-24.04", "x86_64")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if entry.URL != original.URL {
		t.Errorf("expected the built-in (priority 0) entry to win over a lower-priority mirror, got URL %q", entry.URL)
	}
}
