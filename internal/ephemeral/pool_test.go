//go:build linux

package ephemeral

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newPoolManager is newTestManager with pooled clones that wake without a guest.
func newPoolManager(t *testing.T) (*Manager, *fakeEngine, *atomic.Int32) {
	t.Helper()
	orig := poolRetry
	poolRetry = time.Hour // no retry fires during a test
	t.Cleanup(func() { poolRetry = orig })
	m, e := newTestManager(t)
	var woken atomic.Int32
	m.wake = func(context.Context, Clone) error { woken.Add(1); return nil }
	return m, e, &woken
}

// waitPool waits until pool distro has want clones ready and nothing filling.
func waitPool(t *testing.T, m *Manager, distro string, want int) PoolInfo {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		pools, err := m.Pools()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pools {
			m.mu.Lock()
			filling := m.pools[p.ID()] != nil && m.pools[p.ID()].filling
			m.mu.Unlock()
			if p.Distro == distro && p.Ready == want && !filling {
				return p
			}
		}
	}
	t.Fatalf("the %s pool never had %d clones ready", distro, want)
	return PoolInfo{}
}

func (f *fakeEngine) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleted)
}

func TestPoolHandsOutClones(t *testing.T) {
	m, e, woken := newPoolManager(t)
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, 2); err != nil {
		t.Fatal(err)
	}
	info := waitPool(t, m, "ubuntu", 2)
	if info.WarmKey != "kubuntu0" || info.Size != 2 || info.Err != "" {
		t.Errorf("pool %+v", info)
	}
	if e.builds.Load() != 1 {
		t.Errorf("%d warm builds, want the pool to build the snapshot once", e.builds.Load())
	}

	exits, out := collect(t, context.Background(), m, RunRequest{Distros: []string{"ubuntu"}, Argv: []string{"echo", "hi"}})
	if ex := exits["ubuntu"]; ex.Code != 0 || !strings.HasPrefix(ex.CloneID, "eph-P") || out["ubuntu"] != "hi\n" {
		t.Errorf("run on a pooled clone: exit %+v, out %q", ex, out["ubuntu"])
	}
	if woken.Load() != 1 {
		t.Errorf("woken %d times, want once", woken.Load())
	}
	waitPool(t, m, "ubuntu", 2) // refilled

	// A run on another distro restores as before.
	exits, _ = collect(t, context.Background(), m, RunRequest{Distros: []string{"alpine"}, Argv: []string{"true"}})
	if ex := exits["alpine"]; ex.Code != 0 || strings.HasPrefix(ex.CloneID, "eph-P") {
		t.Errorf("run without a pool: %+v", ex)
	}

	// Shrinking deletes the extra clone; size 0 removes the pool and its clones.
	before := len(e.deletedIDs())
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, 1); err != nil {
		t.Fatal(err)
	}
	waitPool(t, m, "ubuntu", 1)
	if got := len(e.deletedIDs()) - before; got != 1 {
		t.Errorf("shrinking deleted %d clones, want 1", got)
	}
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, 0); err != nil {
		t.Fatal(err)
	}
	if pools, _ := m.Pools(); len(pools) != 0 {
		t.Errorf("pools after removing: %+v", pools)
	}
	if got := len(e.deletedIDs()) - before; got != 2 {
		t.Errorf("removing deleted %d clones in all, want 2", got)
	}
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, -1); err == nil {
		t.Error("a negative size was accepted")
	}
}

func TestPoolDropsDeadClones(t *testing.T) {
	m, e, _ := newPoolManager(t)
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, 1); err != nil {
		t.Fatal(err)
	}
	waitPool(t, m, "ubuntu", 1)
	m.wake = func(context.Context, Clone) error { return errors.New("gone") }
	exits, _ := collect(t, context.Background(), m, RunRequest{Distros: []string{"ubuntu"}, Argv: []string{"true"}})
	ex := exits["ubuntu"]
	if ex.Code != 0 || strings.HasPrefix(ex.CloneID, "eph-P") {
		t.Errorf("run after a dead pooled clone: %+v, want a fresh clone", ex)
	}
	if deleted := e.deletedIDs(); len(deleted) == 0 || !strings.HasPrefix(deleted[0], "eph-P") {
		t.Errorf("deleted %v, want the dead pooled clone first", deleted)
	}
}

func TestPoolEvictedForMemory(t *testing.T) {
	m, e, _ := newPoolManager(t)
	if _, err := m.SetPool(WarmParams{Distro: "alpine"}, 1); err != nil {
		t.Fatal(err)
	}
	waitPool(t, m, "alpine", 1)
	// Too little memory until the pooled clone is gone.
	m.memAvailable = func() (int64, error) {
		if len(e.deletedIDs()) > 0 {
			return 1 << 20, nil
		}
		return 100, nil
	}
	exits, _ := collect(t, context.Background(), m, RunRequest{Distros: []string{"ubuntu"}, Argv: []string{"true"}})
	if ex := exits["ubuntu"]; ex.Code != 0 {
		t.Errorf("run that needed the pool's memory: %+v", ex)
	}
	if deleted := e.deletedIDs(); len(deleted) < 1 || !strings.HasPrefix(deleted[0], "eph-P") {
		t.Errorf("deleted %v, want the pooled clone evicted first", deleted)
	}
}

func TestDeleteWarmDropsItsPool(t *testing.T) {
	m, e, _ := newPoolManager(t)
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, 1); err != nil {
		t.Fatal(err)
	}
	info := waitPool(t, m, "ubuntu", 1)
	if _, err := m.DeleteWarm([]string{info.WarmKey}, false); err != nil {
		t.Fatal(err)
	}
	if pools, _ := m.Pools(); len(pools) != 0 {
		t.Errorf("pools after deleting the snapshot: %+v", pools)
	}
	if len(e.deletedIDs()) != 1 {
		t.Errorf("deleted %v, want the pooled clone", e.deletedIDs())
	}
}

func TestStartPools(t *testing.T) {
	m, _, _ := newPoolManager(t)
	if _, err := m.SetPool(WarmParams{Distro: "ubuntu"}, 2); err != nil {
		t.Fatal(err)
	}
	waitPool(t, m, "ubuntu", 2)
	// A new anvild reads the same store and fills the pool again.
	m2 := NewManager(m.Engine, m.Store, m.Dir)
	m2.memAvailable = m.memAvailable
	m2.StartPools()
	waitPool(t, m2, "ubuntu", 2)
}
