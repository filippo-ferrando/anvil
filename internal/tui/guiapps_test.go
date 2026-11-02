package tui

import (
	"os"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/desktop"
)

func TestGUIAppsListAndUnexport(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	path, err := desktop.Write(data, desktop.Spec{Bin: "/bin/anvil", Instance: "dev", Command: []string{"xterm"}}, &anvilv1.GUIAppInfoReply{})
	if err != nil {
		t.Fatal(err)
	}

	m := model{screen: screenGUIApps, guiApps: newGUIAppsModel()}
	next, _ := m.Update(loadGUIApps()())
	m = next.(model)
	e := m.guiApps.selected()
	if e == nil || e.Instance != "dev" || e.App != "xterm" || e.File != path {
		t.Fatalf("selected %+v", e)
	}

	// Any key but y or enter cancels the confirmation.
	next, _ = m.Update(key("x"))
	m = next.(model)
	if m.guiApps.confirmRemove == nil || !strings.Contains(m.guiApps.View(), "Remove xterm (dev)") {
		t.Fatalf("expected a confirmation, view:\n%s", m.guiApps.View())
	}
	next, cmd := m.Update(key("n"))
	m = next.(model)
	if m.guiApps.confirmRemove != nil || cmd != nil {
		t.Fatal("expected n to cancel")
	}

	next, _ = m.Update(key("x"))
	next, cmd = next.(model).Update(key("y"))
	m = next.(model)
	if cmd == nil {
		t.Fatal("expected y to unexport")
	}
	next, cmd = m.Update(cmd())
	m = next.(model)
	if !strings.Contains(m.status, "removed "+path) || cmd == nil {
		t.Fatalf("status %q", m.status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("entry still there: %v", err)
	}
	next, _ = m.Update(cmd())
	if n := len(next.(model).guiApps.list.Items()); n != 0 {
		t.Fatalf("%d items left after unexport", n)
	}

	// Removing it again reports the missing entry.
	msg := removeGUIApp(*e)().(guiAppRemovedMsg)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "no exported app xterm for dev") {
		t.Errorf("second remove: %v", msg.err)
	}
}

func TestRelaunchGUIAppRunsExec(t *testing.T) {
	msg := relaunchGUIApp(desktop.Export{Exec: []string{"sh", "-c", "echo started"}})().(guiLaunchedMsg)
	if msg.err != nil || msg.output != "started" {
		t.Errorf("got %+v", msg)
	}
	if msg := relaunchGUIApp(desktop.Export{File: "x.desktop"})().(guiLaunchedMsg); msg.err == nil {
		t.Error("expected an entry without Exec to fail")
	}
}

func TestScreenDigitEveryEntry(t *testing.T) {
	if len(screenKeys) < len(screenOrder) {
		t.Fatalf("%d jump keys for %d sidebar entries", len(screenKeys), len(screenOrder))
	}
	if screenDigit(9) != "0" || screenDigit(0) != "1" || screenDigit(12) != "c" || screenDigit(len(screenKeys)) != " " {
		t.Error("unexpected sidebar keys")
	}
	if screenOrder[len(screenOrder)-1] != screenTasks {
		t.Errorf("last sidebar entry %v, want Tasks", screenOrder[len(screenOrder)-1])
	}
}

func TestSidebarFitsShortTerminal(t *testing.T) {
	for _, h := range []int{24, 50} {
		m := model{height: h, screen: screenInstances, sidebarFocused: true}
		v := m.sidebarView()
		for _, s := range screenOrder {
			if !strings.Contains(v, screenLabels[s]) {
				t.Errorf("height %d: %s missing from the sidebar", h, screenLabels[s])
			}
		}
		if h == 50 && !strings.Contains(v, "AUTOMATION") {
			t.Errorf("height %d: no section headers:\n%s", h, v)
		}
	}
}
