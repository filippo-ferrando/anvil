package client

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestParseCollect(t *testing.T) {
	for in, want := range map[string]CollectSpec{
		"dist":              {Guest: "dist", Host: "."},
		"dist:out":          {Guest: "dist", Host: "out"},
		"/work/**/*.xml:r/": {Guest: "/work/**/*.xml", Host: "r/"},
		"dist:":             {Guest: "dist", Host: "."},
	} {
		if got, err := ParseCollect(in); err != nil || got != want {
			t.Errorf("ParseCollect(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", ":out", " :x"} {
		if _, err := ParseCollect(bad); err == nil {
			t.Errorf("ParseCollect(%q) was accepted", bad)
		}
	}
	spec := CollectSpec{Guest: "dist", Host: "out"}
	if got := CollectDir(spec, "ubuntu-24.04", false); got != "out" {
		t.Errorf("CollectDir single = %q", got)
	}
	if got := CollectDir(spec, "a/b", true); got != filepath.Join("out", "a_b") {
		t.Errorf("CollectDir per distro = %q", got)
	}
}

func testTar(t *testing.T, files map[string]string, links map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, data := range files {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./" + name, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write([]byte(data))
	}
	for name, target := range links {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target})
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCollector(t *testing.T) {
	root := t.TempDir()
	c := NewCollector([]string{"dist", "*.xml"}, func(i int, distro string) string {
		return filepath.Join(root, distro, []string{"d", "x"}[i])
	})
	ev := func(clone, distro string, index int, data []byte, done bool, warning string) *anvilv1.RunCollect {
		return &anvilv1.RunCollect{CloneId: clone, Distro: distro, Index: int32(index), Data: data, Done: done, Warning: warning}
	}
	tarA := testTar(t, map[string]string{"bin/app": "A"}, map[string]string{"latest": "bin/app"})
	tarB := testTar(t, map[string]string{"bin/app": "B"}, nil)

	// Two clones' streams interleave, each in several chunks.
	half := len(tarA) / 2
	for _, e := range []*anvilv1.RunCollect{ev("c1", "ubuntu", 0, tarA[:half], false, ""), ev("c2", "alpine", 0, tarB[:10], false, "")} {
		if res := c.Handle(e); res != nil {
			t.Fatalf("result before done: %+v", res)
		}
	}
	res := c.Handle(ev("c1", "ubuntu", 0, tarA[half:], true, ""))
	if res == nil || res.Err != nil || res.Path != "dist" || res.Dir != filepath.Join(root, "ubuntu", "d") {
		t.Fatalf("ubuntu result = %+v", res)
	}
	if res := c.Handle(ev("c2", "alpine", 0, tarB[10:], true, "")); res == nil || res.Err != nil {
		t.Fatalf("alpine result = %+v", res)
	}
	for path, want := range map[string]string{"ubuntu/d/bin/app": "A", "ubuntu/d/latest": "A", "alpine/d/bin/app": "B"} {
		if got, err := os.ReadFile(filepath.Join(root, path)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", path, got, err)
		}
	}

	// A path with nothing sent back carries its warning and makes no directory.
	res = c.Handle(ev("c1", "ubuntu", 1, nil, true, "*.xml: no match"))
	if res == nil || res.Err == nil || !strings.Contains(res.Err.Error(), "no match") || res.Dir != "" {
		t.Errorf("warning result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "ubuntu", "x")); !os.IsNotExist(err) {
		t.Errorf("a path with no match made its directory: %v", err)
	}

	// Collecting into the same directory again replaces the earlier files and links.
	c2 := NewCollector([]string{"dist"}, func(int, string) string { return filepath.Join(root, "ubuntu", "d") })
	tarA2 := testTar(t, map[string]string{"bin/app": "A2"}, map[string]string{"latest": "bin/app"})
	if res := c2.Handle(ev("c3", "ubuntu", 0, tarA2, true, "")); res == nil || res.Err != nil {
		t.Fatalf("second collect = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "ubuntu", "d", "latest")); string(got) != "A2" {
		t.Errorf("after a second collect, latest = %q", got)
	}

	// An entry that would leave the directory fails the path, not the run.
	evil := testTar(t, map[string]string{"../../escape": "x"}, nil)
	res = c2.Handle(ev("c4", "ubuntu", 0, evil, true, ""))
	if res == nil || res.Err == nil {
		t.Errorf("an escaping entry was accepted: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "..", "escape")); err == nil {
		t.Error("an entry escaped the collect directory")
	}

	// Close ends an extraction cut off mid-stream without blocking.
	c.Handle(ev("c5", "ubuntu", 0, tarA[:half], false, ""))
	c.Close()
	if len(c.open) != 0 {
		t.Errorf("open after Close: %d", len(c.open))
	}
}
