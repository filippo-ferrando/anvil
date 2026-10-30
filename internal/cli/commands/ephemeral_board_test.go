package commands

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestStatusBoard(t *testing.T) {
	var out bytes.Buffer
	b := newStatusBoard(&out, []string{"debian-13", "alpine"})
	var above []string
	keep := func(d, line string) { above = append(above, d+" "+line) }

	b.set("debian-13", "building the warm snapshot", keep)
	b.set("alpine", "restoring a clone", keep)
	if b.drawn != 2 {
		t.Fatalf("drawn %d", b.drawn)
	}
	// An update of the same step redraws in place: up 2 lines, erase, 2 lines again.
	out.Reset()
	b.set("alpine", "restoring a clone", keep)
	if got := out.String(); !strings.HasPrefix(got, "\x1b[2A\r\x1b[J") || strings.Count(got, "\n") != 2 {
		t.Fatalf("redraw %q", got)
	}
	// A step that took a while stays above the board once the next one starts.
	b.steps["debian-13"].since = time.Now().Add(-3 * time.Second)
	b.set("debian-13", "running: make test", keep)
	if len(above) != 1 || !strings.Contains(above[0], "building the warm snapshot  (3s)") {
		t.Fatalf("above %q", above)
	}
	b.set("alpine", "running: make test", keep) // a quick step leaves nothing behind
	if len(above) != 1 {
		t.Fatalf("above %q", above)
	}

	out.Reset()
	b.print(func() { out.WriteString("output\n") })
	if got := out.String(); !strings.Contains(got, "\x1b[J"+"output\n") || !strings.HasSuffix(got, "\x1b[0m\n") {
		t.Fatalf("print %q", got)
	}
	b.done("alpine")
	if b.drawn != 1 {
		t.Fatalf("drawn after done %d", b.drawn)
	}
	b.stop()
	out.Reset()
	b.set("debian-13", "x", keep)
	b.redraw()
	if out.Len() != 0 || b.drawn != 0 {
		t.Fatalf("drew after stop: %q", out.String())
	}
}
