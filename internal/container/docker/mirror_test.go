package docker

import "testing"

func TestSplitRegistryHost(t *testing.T) {
	cases := []struct {
		ref, wantRegistry, wantRest string
	}{
		{"nginx", "", "nginx"},
		{"nginx:alpine", "", "nginx:alpine"},
		{"library/nginx", "", "library/nginx"},
		{"bitnami/postgres:16", "", "bitnami/postgres:16"},
		{"quay.io/prometheus/prometheus", "quay.io", "prometheus/prometheus"},
		{"myregistry:5000/nginx:alpine", "myregistry:5000", "nginx:alpine"},
		{"localhost/nginx", "localhost", "nginx"},
		{"localhost:5000/nginx", "localhost:5000", "nginx"},
	}
	for _, tc := range cases {
		registry, rest := splitRegistryHost(tc.ref)
		if registry != tc.wantRegistry || rest != tc.wantRest {
			t.Errorf("splitRegistryHost(%q) = (%q, %q), want (%q, %q)", tc.ref, registry, rest, tc.wantRegistry, tc.wantRest)
		}
	}
}

func TestResolveMirrorRewritesDockerHubByDefault(t *testing.T) {
	mirrors := []RegistryMirror{{Registry: "mirror.corp"}} // MirrorOf unset -> docker.io
	got := ResolveMirror("nginx:alpine", mirrors)
	want := "mirror.corp/nginx:alpine"
	if got != want {
		t.Errorf("ResolveMirror = %q, want %q", got, want)
	}
}

func TestResolveMirrorRewritesExplicitUpstream(t *testing.T) {
	mirrors := []RegistryMirror{{Registry: "mirror.corp/quay-cache", MirrorOf: "quay.io"}}
	got := ResolveMirror("quay.io/prometheus/prometheus:latest", mirrors)
	want := "mirror.corp/quay-cache/prometheus/prometheus:latest"
	if got != want {
		t.Errorf("ResolveMirror = %q, want %q", got, want)
	}
}

func TestResolveMirrorLeavesRefAloneWhenNoMirrorMatches(t *testing.T) {
	mirrors := []RegistryMirror{{Registry: "mirror.corp", MirrorOf: "quay.io"}}
	ref := "nginx:alpine" // implicit docker.io, mirror is for quay.io
	if got := ResolveMirror(ref, mirrors); got != ref {
		t.Errorf("ResolveMirror = %q, want unchanged %q", got, ref)
	}
}

func TestResolveMirrorFirstMatchWins(t *testing.T) {
	// ResolveMirror takes the first matching mirror in the slice.
	mirrors := []RegistryMirror{
		{Registry: "high-priority.corp"},
		{Registry: "low-priority.corp"},
	}
	got := ResolveMirror("nginx:alpine", mirrors)
	want := "high-priority.corp/nginx:alpine"
	if got != want {
		t.Errorf("ResolveMirror = %q, want %q", got, want)
	}
}
