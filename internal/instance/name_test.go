package instance

import (
	"strings"
	"testing"
)

func TestNameFromImage(t *testing.T) {
	cases := map[string]string{
		"ubuntu:24.04":                   "ubuntu-24.04",
		"debian-13":                      "debian-13",
		"docker.io/library/nginx:alpine": "nginx-alpine",
		"localhost:5000/team/app:v1":     "app-v1",
		"redis@sha256:abcdef":            "redis",
		"ghcr.io/x/y/z":                  "z",
		"_weird::name__":                 "weird-name",
		"":                               "instance",
		":::":                            "instance",
		strings.Repeat("a", 80):          strings.Repeat("a", 63),
	}
	for in, want := range cases {
		got := NameFromImage(in)
		if got != want {
			t.Errorf("NameFromImage(%q) = %q, want %q", in, got, want)
		}
		if err := ValidateName(got); err != nil {
			t.Errorf("NameFromImage(%q) = %q is not valid: %v", in, got, err)
		}
	}
}

func TestUniqueName(t *testing.T) {
	used := map[string]bool{"nginx": true, "nginx-2": true}
	got, err := UniqueName("nginx", func(n string) bool { return used[n] })
	if err != nil || got != "nginx-3" {
		t.Fatalf("got %q, %v; want nginx-3", got, err)
	}
	got, err = UniqueName("free", func(n string) bool { return used[n] })
	if err != nil || got != "free" {
		t.Fatalf("got %q, %v; want free", got, err)
	}
	long := strings.Repeat("b", 63)
	got, err = UniqueName(long, func(n string) bool { return n == long })
	if err != nil || len(got) > 63 || !strings.HasSuffix(got, "-2") {
		t.Fatalf("got %q, %v; want a 63-char name ending in -2", got, err)
	}
}
