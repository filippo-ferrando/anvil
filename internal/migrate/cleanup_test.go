package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// containerWithVolumes returns a container spec with n bind-mounted dirs holding a file each.
func containerWithVolumes(t *testing.T, n int) *instance.Spec {
	t.Helper()
	spec := &instance.Spec{ID: "01JCT", Name: "app", Kind: instance.KindContainer, Container: &instance.ContainerSpec{ImageRef: "nginx"}}
	for range n {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		spec.Container.Volumes = append(spec.Container.Volumes, instance.VolumeMount{HostPath: dir, ContainerPath: "/data"})
	}
	return spec
}

// stagedLeft lists what is still in the fake remote staging dir.
func stagedLeft(t *testing.T, remote string) []string {
	t.Helper()
	entries, err := os.ReadDir(remote)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A failed import leaves nothing staged on the target.
func TestMigrateSpecRemovesStagedWhenImportFails(t *testing.T) {
	remote := t.TempDir()
	localSSH(t, func(cmd string) string {
		if cmd == "anvil migrate-import" {
			return "cat >/dev/null; echo 'MIGRATE_FAIL boom'"
		}
		return strings.ReplaceAll(cmd, remoteStagingDir, remote)
	})
	m := &Manager{}
	_, err := m.migrateSpec(context.Background(), target{}, containerWithVolumes(t, 2), "app", nil, remoteCaps{}, func(string) {})
	if err == nil || !targetMayHave(err) {
		t.Fatalf("got %v, want a target launch failure", err)
	}
	if left := stagedLeft(t, remote); len(left) != 0 {
		t.Errorf("staged data left on the target: %v", left)
	}
}

func TestMigrateSpecRemovesPartialUpload(t *testing.T) {
	remote := t.TempDir()
	localSSH(t, func(cmd string) string {
		cmd = strings.ReplaceAll(cmd, remoteStagingDir, remote)
		if strings.Contains(cmd, "anvil-migrate-data-01JCT-1") && strings.Contains(cmd, "tar") {
			// The second dir is created, then the upload breaks.
			return "mkdir -p " + shQuote(remote+"/anvil-migrate-data-01JCT-1") + "; cat >/dev/null; exit 1"
		}
		return cmd
	})
	m := &Manager{}
	_, err := m.migrateSpec(context.Background(), target{}, containerWithVolumes(t, 2), "app", nil, remoteCaps{}, func(string) {})
	if err == nil || targetMayHave(err) {
		t.Fatalf("got %v, want an upload failure", err)
	}
	if left := stagedLeft(t, remote); len(left) != 0 {
		t.Errorf("staged data left on the target: %v", left)
	}
}

type startRecorder struct {
	Instances
	started []string
	err     error
}

func (s *startRecorder) Start(_ context.Context, names []string) error {
	s.started = append(s.started, names...)
	return s.err
}

func TestRestartSourcesOnlyRestartsRunning(t *testing.T) {
	rec := &startRecorder{err: errors.New("no")}
	m := &Manager{Instances: rec}
	var notes []string
	m.restartSources(context.Background(), []*instance.Spec{
		{Name: "a", State: instance.StateRunning},
		{Name: "b", State: instance.StateStopped},
	}, func(s string) { notes = append(notes, s) })
	if len(rec.started) != 1 || rec.started[0] != "a" {
		t.Errorf("started %v, want only a", rec.started)
	}
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "warning") {
		t.Errorf("a failed restart is not reported: %v", notes)
	}
}

func TestRollbackUncertainOneByOne(t *testing.T) {
	var calls int
	localSSH(t, func(cmd string) string {
		calls++
		return "cat >/dev/null; exit 1"
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rollbackUncertain(ctx, target{}, []string{"a", "b"})
	if calls != 2 {
		t.Errorf("%d rollback calls, want one per name so a missing one can't block the rest", calls)
	}
}
