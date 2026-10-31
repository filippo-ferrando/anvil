package tui

import (
	"fmt"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestLogsRenderCoalesced(t *testing.T) {
	m := model{screen: screenLogs, logs: newLogsModel(&anvilv1.Instance{Name: "web"}, 80, 10)}
	chunk := func(s string, done bool) {
		t.Helper()
		next, _ := m.Update(logsStreamMsg{streamStep[[]byte]{result: []byte(s), done: done}})
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
