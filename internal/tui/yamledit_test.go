package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// classOf returns the class of the first rune of sub in line, highlighting lines together.
func classOf(t *testing.T, lines []string, line int, sub string) int {
	t.Helper()
	i := strings.Index(lines[line], sub)
	if i < 0 {
		t.Fatalf("%q not in %q", sub, lines[line])
	}
	return highlightYAML(lines)[line][len([]rune(lines[line][:i]))]
}

func TestHighlightYAML(t *testing.T) {
	lines := []string{
		"#cloud-config",
		"package_update: true # refresh",
		"packages: [nginx, \"curl\"]",
		"write_files:",
		"  - path: /etc/motd",
		"    content: |",
		"      hello: not a key",
		"    owner: root:root",
		"image: postgres:${PG_TAG}",
		"url: http://x",
	}
	cases := []struct {
		line int
		sub  string
		want int
	}{
		{0, "#cloud-config", tokHeader},
		{1, "package_update", tokKey},
		{1, "true", tokConst},
		{1, "# refresh", tokComment},
		{2, "[", tokPunct},
		{2, "nginx", tokString},
		{2, "\"curl\"", tokString},
		{4, "-", tokPunct},
		{4, "path", tokKey},
		{5, "|", tokPunct},
		{6, "hello", tokString}, // inside the block scalar
		{7, "owner", tokKey},    // the block ended
		{7, "root:root", tokString},
		{8, "${PG_TAG}", tokVar},
		{9, "http://x", tokString},
	}
	for _, tc := range cases {
		if got := classOf(t, lines, tc.line, tc.sub); got != tc.want {
			t.Errorf("line %d %q: class %d, want %d", tc.line, tc.sub, got, tc.want)
		}
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestYAMLEditorView(t *testing.T) {
	e := newYAMLEditor("", "valid cloud-config", checkCloudInit)
	e.SetWidth(40)
	e.SetHeight(6)
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "k%d: v\n", i) // unknown keys, so every line gets a warning
	}
	b.WriteString("runcmd: reboot") // an error, with the cursor on it
	e.SetValue(b.String())
	e.Focus()
	view := func() []string { return strings.Split(ansiRe.ReplaceAllString(e.View(), ""), "\n") }
	if rows := view(); !strings.Contains(rows[0], "  1 #cloud-config") {
		t.Fatalf("a long document should open at its top, got %q", rows[0])
	}
	for e.Line() < e.LineCount()-1 {
		e, _ = e.Update(tea.KeyMsg{Type: tea.KeyDown})
	}

	rows := view()
	if len(rows) != 6 {
		t.Fatalf("view has %d rows, want the editor height 6:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	if !strings.Contains(rows[4], " 22 runcmd") || !strings.HasPrefix(rows[4], "✕") {
		t.Errorf("last text row should be line 22 marked as an error, got %q", rows[4])
	}
	if !strings.HasPrefix(rows[0], "!") {
		t.Errorf("an unknown key line should be marked as a warning, got %q", rows[0])
	}
	if !strings.Contains(rows[5], "line 22: runcmd must be a list") {
		t.Errorf("status should show the cursor line's problem, got %q", rows[5])
	}
}

func TestYAMLEditorKeepsLongDocuments(t *testing.T) {
	e := newYAMLEditor("", "", nil)
	long := strings.Repeat("a: 1\n", 150)
	e.SetValue(long)
	if e.Value() != long {
		t.Fatalf("SetValue kept %d of 151 lines", e.LineCount())
	}
}
