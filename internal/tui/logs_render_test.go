package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestLogsRenderCoalesced(t *testing.T) {
	m := model{screen: screenLogs, logs: newLogsModel(&anvilv1.Instance{Name: "web"}, 80, 10)}
	chunk := func(s string, done bool) {
		t.Helper()
		next, _ := m.Update(logsStreamMsg{streamStep: streamStep[[]byte]{result: []byte(s), done: done}})
		m = next.(model)
	}
	for i := range 50 {
		chunk(fmt.Sprintf("line %d\n", i), false)
	}
	if !m.logs.dirty || !m.logs.queued {
		t.Fatalf("after 50 chunks: dirty %v, queued %v; want one render queued", m.logs.dirty, m.logs.queued)
	}
	if strings.Contains(m.logs.viewport.View(), "line 49") {
		t.Error("the viewport was filled on a chunk, not on the render tick")
	}
	next, _ := m.Update(logsRenderMsg{})
	m = next.(model)
	if m.logs.dirty || m.logs.queued || !strings.Contains(m.logs.viewport.View(), "line 49") {
		t.Errorf("after the render: dirty %v, queued %v, view:\n%s", m.logs.dirty, m.logs.queued, m.logs.viewport.View())
	}
	// The end of the stream shows at once, the line still waiting for its newline included.
	chunk("last line, no newline", true)
	if !strings.Contains(m.logs.viewport.View(), "last line, no newline") {
		t.Errorf("end of stream not shown:\n%s", m.logs.viewport.View())
	}
}

func TestLogsDropsStaleStream(t *testing.T) {
	m := model{screen: screenLogs, logs: newLogsModel(&anvilv1.Instance{Name: "web"}, 80, 10)}
	m.logs.gen = 2
	next, _ := m.Update(logsStreamMsg{streamStep: streamStep[[]byte]{result: []byte("old instance\n")}, gen: 1})
	m = next.(model)
	next, _ = m.Update(logsStreamMsg{streamStep: failed[[]byte](context.Canceled), gen: 1})
	m = next.(model)
	if len(m.logs.lines) != 0 || !m.logs.streaming || m.status != "" {
		t.Fatalf("stale stream applied: lines %q, streaming %v, status %q", m.logs.lines, m.logs.streaming, m.status)
	}
	next, _ = m.Update(logsStreamMsg{streamStep: streamStep[[]byte]{result: []byte("new instance\n")}, gen: 2})
	if got := next.(model).logs.lines; len(got) != 1 || got[0] != "new instance" {
		t.Fatalf("lines %q", got)
	}
}

func TestCleanLine(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"plain", []string{"plain"}},
		{"crlf\r", []string{"crlf"}},
		// systemd ends its status lines with "\r\r\n".
		{"[\x1b[0;32m  OK  \x1b[0m] Started foo.\r\r", []string{"[  OK  ] Started foo."}},
		{"\rLoading Linux ...", []string{"Loading Linux ..."}},
		{"50%\r75%\r100%", []string{"100%"}},
		// A kernel message cutting into a status line gets its own line.
		{"Starting foo...\r[    2.322103] ACPI: bus registered", []string{"Starting foo...", "[    2.322103] ACPI: bus registered"}},
		{"\r\r", nil},
		{"\x1b[2J\x1b[001;001H", nil},
	}
	for _, c := range cases {
		if got := cleanLine(c.raw); !slices.Equal(got, c.want) {
			t.Errorf("cleanLine(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
