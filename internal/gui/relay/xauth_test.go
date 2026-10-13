//go:build linux

package relay

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestXAuthority_RoundTrip(t *testing.T) {
	entries := []XAuthEntry{
		{Family: familyLocal, Address: []byte("host"), Number: "0", Name: "MIT-MAGIC-COOKIE-1", Data: []byte{1, 2, 3}},
		{Family: 0, Address: []byte{10, 0, 0, 1}, Number: "1", Name: "MIT-MAGIC-COOKIE-1", Data: []byte{4}},
	}
	var buf bytes.Buffer
	if err := WriteXAuthority(&buf, entries); err != nil {
		t.Fatal(err)
	}
	got, err := ReadXAuthority(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("got %+v\nwant %+v", got, entries)
	}
}

func TestReadXAuthority_Truncated(t *testing.T) {
	if _, err := ReadXAuthority(bytes.NewReader([]byte{1, 0, 0, 5, 'a'})); err == nil {
		t.Fatal("expected an error for a truncated record")
	}
}

func TestCookieFor_SkipsNetworkFamilies(t *testing.T) {
	entries := []XAuthEntry{
		{Family: 0, Number: "0", Data: []byte("tcp")},
		{Family: familyLocal, Number: "1", Data: []byte("other")},
		{Family: familyLocal, Number: "0", Data: []byte("local")},
	}
	e, ok := CookieFor(entries, 0)
	if !ok || string(e.Data) != "local" {
		t.Fatalf("got %+v %v", e, ok)
	}
	if _, ok := CookieFor(entries, 7); ok {
		t.Fatal("display 7 has no cookie")
	}
}

func TestWriteGuestXAuthority(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "host")
	dst := filepath.Join(dir, "guest")

	if ok, err := WriteGuestXAuthority(src, 0, dst, 0); ok || err != nil {
		t.Fatalf("missing source file: got %v %v, want false nil", ok, err)
	}

	var buf bytes.Buffer
	_ = WriteXAuthority(&buf, []XAuthEntry{{Family: familyLocal, Address: []byte("laptop"), Number: "2", Name: "MIT-MAGIC-COOKIE-1", Data: []byte("secret")}})
	if err := os.WriteFile(src, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := WriteGuestXAuthority(src, 2, dst, 0)
	if !ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
	f, _ := os.Open(dst)
	defer f.Close()
	got, err := ReadXAuthority(f)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
	want := XAuthEntry{Family: familyWild, Address: []byte{}, Number: "0", Name: "MIT-MAGIC-COOKIE-1", Data: []byte("secret")}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %+v, want %+v", got[0], want)
	}
}

func TestParseDisplay(t *testing.T) {
	for in, want := range map[string]int{":0": 0, ":1.0": 1, "unix:12": 12} {
		got, err := ParseDisplay(in)
		if err != nil || got != want {
			t.Errorf("ParseDisplay(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "remote:0", ":x", "localhost:10.0"} {
		if _, err := ParseDisplay(in); err == nil {
			t.Errorf("ParseDisplay(%q) should fail", in)
		}
	}
}

func TestWaylandSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-9")
	if _, err := WaylandSocket(); err == nil {
		t.Fatal("expected an error with no socket present")
	}
	listen, cancel, _ := startRelay(t, filepath.Join(dir, "nothing"))
	defer cancel()
	t.Setenv("WAYLAND_DISPLAY", listen) // absolute form
	got, err := WaylandSocket()
	if err != nil || got != listen {
		t.Fatalf("got %q %v", got, err)
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	if _, err := WaylandSocket(); err == nil {
		t.Fatal("expected an error with WAYLAND_DISPLAY unset")
	}
}
