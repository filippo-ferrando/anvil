package ephemeral

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
)

func TestSetupKey(t *testing.T) {
	base := WarmSpec{Key: "base1", Distro: "ubuntu"}
	setup := func(script string, files ...SetupFile) *SetupScript {
		return &SetupScript{Name: "setup.sh", Script: []byte(script), Files: files}
	}
	key := func(s *SetupScript) string {
		t.Helper()
		spec, err := withSetup(base, s)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Base != "base1" || spec.SetupName != "setup.sh" || len(spec.Key) != 16 {
			t.Errorf("spec = %+v", spec)
		}
		return spec.Key
	}
	a := key(setup("make deps", SetupFile{"go.sum", []byte("v1")}, SetupFile{"go.mod", []byte("m")}))
	if b := key(setup("make deps", SetupFile{"go.mod", []byte("m")}, SetupFile{"go.sum", []byte("v1")})); a != b {
		t.Error("the files' order changed the key")
	}
	if b := key(setup("make deps", SetupFile{"go.sum", []byte("v2")}, SetupFile{"go.mod", []byte("m")})); a == b {
		t.Error("a changed key file kept the key")
	}
	if b := key(setup("make deps2", SetupFile{"go.sum", []byte("v1")}, SetupFile{"go.mod", []byte("m")})); a == b {
		t.Error("a changed script kept the key")
	}
	other := base
	other.Key = "base2"
	if spec, _ := withSetup(other, setup("make deps", SetupFile{"go.sum", []byte("v1")}, SetupFile{"go.mod", []byte("m")})); spec.Key == a {
		t.Error("a new base kept the key")
	}
	for _, bad := range []*SetupScript{
		setup("  \n"),
		setup("x", SetupFile{"../etc/passwd", nil}),
		setup("x", SetupFile{"/abs", nil}),
		setup("x", SetupFile{"a", nil}, SetupFile{"./a", nil}),
		setup("x", SetupFile{setupScriptName, nil}),
		setup("x", SetupFile{"big", make([]byte, maxSetupBytes)}),
	} {
		if _, err := withSetup(base, bad); err == nil {
			t.Errorf("setup %+v was accepted", bad.Files)
		}
	}
}

// TestSetupBuild runs the setup script against a guest server on this host.
func TestSetupBuild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	m, e := newTestManager(t)
	e.runSetup = true
	work := filepath.Join(t.TempDir(), "work")
	setWorkDir(t, work)
	out := filepath.Join(t.TempDir(), "setup-ran")
	setup := &SetupScript{
		Name:   "ci/setup.sh",
		Script: []byte("#!/bin/sh\nset -e\ncat locks/go.sum > " + out + "\npwd >> " + out + "\n"),
		Files:  []SetupFile{{Name: "locks/go.sum", Data: []byte("sum-v1\n")}},
	}
	var steps []string
	rec, spec, err := m.Warm(context.Background(), WarmParams{Distro: "ubuntu", CPUs: 2, Setup: setup}, false, func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "sum-v1\n"+work+"\n" {
		t.Errorf("the script wrote %q", got)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("/work stayed after the setup: %v", err)
	}
	if rec.Base != "kubuntu2" || rec.Setup != "ci/setup.sh" || rec.Key != spec.Key || spec.Setup == nil {
		t.Errorf("record = %+v", rec)
	}
	if e.builds.Load() != 2 || e.setups.Load() != 1 {
		t.Errorf("%d builds, %d with a setup; want the base, then the setup", e.builds.Load(), e.setups.Load())
	}
	if !slices.ContainsFunc(steps, func(s string) bool { return strings.Contains(s, "running the setup script ci/setup.sh") }) {
		t.Errorf("steps = %q", steps)
	}
	// The same script and files reuse it.
	if again, _, err := m.Warm(context.Background(), WarmParams{Distro: "ubuntu", CPUs: 2, Setup: setup}, false, nil); err != nil || again.Key != rec.Key || e.builds.Load() != 2 {
		t.Errorf("reuse = %+v, %v, %d builds", again, err, e.builds.Load())
	}

	// A failing script fails the build with the end of its output, and leaves no record.
	bad := &SetupScript{Name: "bad.sh", Script: []byte("echo installing; echo broken >&2; exit 4")}
	_, _, err = m.Warm(context.Background(), WarmParams{Distro: "ubuntu", CPUs: 2, Setup: bad}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "exited 4") || !strings.Contains(err.Error(), "broken") {
		t.Errorf("failing setup = %v", err)
	}
	if list, _ := m.ListWarm(); len(list) != 2 {
		t.Errorf("records after a failed setup: %d, want 2", len(list))
	}
}

func setWorkDir(t *testing.T, dir string) {
	t.Helper()
	oldGuest, old := guestexec.WorkDir, workDir
	guestexec.WorkDir, workDir = dir, dir
	t.Cleanup(func() { guestexec.WorkDir, workDir = oldGuest, old })
}

func TestWarmByKeyAndDerivedDelete(t *testing.T) {
	m, e := newTestManager(t)
	ctx := context.Background()
	setup := &SetupScript{Name: "s.sh", Script: []byte("true")}
	rec, _, err := m.Warm(ctx, WarmParams{Distro: "ubuntu", CPUs: 2, Setup: setup}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// "@<key prefix>" picks it without building anything.
	got, spec, err := m.Warm(ctx, WarmParams{Distro: "@" + rec.Key[:6]}, false, nil)
	if err != nil || got.Key != rec.Key || spec.Key != rec.Key || spec.Distro != "ubuntu" || spec.CPUs != 2 || e.builds.Load() != 2 {
		t.Errorf("by key = %+v, %+v, %v, %d builds", got, spec, err, e.builds.Load())
	}
	if _, _, err := m.Warm(ctx, WarmParams{Distro: "@nope"}, false, nil); err == nil {
		t.Error("an unknown key was accepted")
	}
	if _, _, err := m.Warm(ctx, WarmParams{Distro: "@" + rec.Key, Setup: setup}, false, nil); err == nil {
		t.Error("a key with a setup script was accepted")
	}

	// A run on "@key" restores from that snapshot.
	exits, _ := collect(t, ctx, m, RunRequest{Distros: []string{"@" + rec.Key}, Argv: []string{"true"}})
	if ex := exits["@"+rec.Key]; ex.Code != 0 || ex.Err != "" {
		t.Errorf("run on @key = %+v", ex)
	}

	// A pool on it keeps that key.
	info, err := m.SetPool(WarmParams{Distro: "@" + rec.Key[:6]}, 1)
	if err != nil || info.Distro != "@"+rec.Key || info.CPUs != 2 {
		t.Fatalf("SetPool = %+v, %v", info, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pools, _ := m.Pools(); len(pools) == 1 && pools[0].Ready == 1 && pools[0].WarmKey == rec.Key {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pools, _ := m.Pools(); len(pools) != 1 || pools[0].Ready != 1 || pools[0].WarmKey != rec.Key {
		t.Errorf("pool on @key = %+v", pools)
	}

	// Removing the base removes what is built on it, and the pool with it.
	deleted, err := m.DeleteWarm([]string{rec.Base}, false)
	if err != nil || !slices.Equal(deleted, []string{rec.Base, rec.Key}) {
		t.Errorf("DeleteWarm = %v, %v", deleted, err)
	}
	if list, _ := m.ListWarm(); len(list) != 0 {
		t.Errorf("left after deleting the base: %+v", list)
	}
}

func TestSaveClone(t *testing.T) {
	m, e := newTestManager(t)
	ctx := context.Background()
	exits, _ := collect(t, ctx, m, RunRequest{Distros: []string{"alpine"}, Argv: []string{"false"}, KeepFailed: true})
	kept := exits["alpine"].CloneID
	if !exits["alpine"].Kept {
		t.Fatalf("no kept clone: %+v", exits)
	}
	base, _ := m.Store.GetWarm("kalpine0")
	rec, err := m.SaveClone(ctx, kept[:8])
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Saved || rec.Base != base.Key || rec.Setup != "saved from "+kept || rec.Distro != "alpine" || rec.Key == base.Key || !m.complete(rec.Key) {
		t.Errorf("saved = %+v", rec)
	}
	if len(m.Clones()) != 1 {
		t.Error("saving deleted the clone")
	}
	if _, err := m.SaveClone(ctx, "eph-nope"); err == nil {
		t.Error("an unknown clone was saved")
	}
	e.saveErr = errors.New("disk full")
	if _, err := m.SaveClone(ctx, kept); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("failed save = %v", err)
	}
	if list, _ := m.ListWarm(); len(list) != 2 {
		t.Errorf("records = %d, want the base and one saved", len(list))
	}
}
