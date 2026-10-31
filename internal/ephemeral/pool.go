package ephemeral

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// poolRetry is how long a pool whose refill failed waits before trying again.
var poolRetry = 30 * time.Second

// pool is the running side of a store.WarmPool: its clones restored and waiting for a run.
// Its fields are guarded by Manager.mu.
type pool struct {
	cfg     store.WarmPool
	key     string // the warm snapshot its clones come from, once known
	ready   []Clone
	filling bool
	err     string // why the last refill stopped, "" when it didn't fail
	gone    bool   // removed: a refill in progress deletes what it restores
}

// PoolInfo describes a pool and how many of its clones are ready.
type PoolInfo struct {
	store.WarmPool
	WarmKey string
	Ready   int
	Err     string
}

// SetPool keeps size clones of p's warm snapshot restored ahead of time; size 0 removes the pool.
func (m *Manager) SetPool(p WarmParams, size int) (PoolInfo, error) {
	if size < 0 {
		return PoolInfo{}, errors.New("a pool size can't be negative")
	}
	if p.App != "" {
		return PoolInfo{}, errors.New("warm snapshots with an app have no pool")
	}
	e, err := m.engine()
	if err != nil {
		return PoolInfo{}, err
	}
	var cfg store.WarmPool
	if key, ok := warmRef(p.Distro); ok {
		// One snapshot by key: its pool follows that snapshot only, never a rebuild.
		w, err := m.resolveWarm(key)
		if err != nil {
			return PoolInfo{}, err
		}
		cfg = store.WarmPool{Distro: "@" + w.Key, Arch: w.Arch, CPUs: w.CPUs, MemoryMiB: w.MemoryMiB, Size: size}
	} else {
		s, err := e.WarmSpec(p)
		if err != nil {
			return PoolInfo{}, err
		}
		cfg = store.WarmPool{Distro: s.Distro, Arch: s.Arch, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, Size: size}
	}
	if size == 0 {
		if err := m.Store.DeleteWarmPool(cfg.ID()); err != nil {
			return PoolInfo{}, err
		}
		m.mu.Lock()
		pl := m.pools[cfg.ID()]
		delete(m.pools, cfg.ID())
		m.mu.Unlock()
		if pl != nil {
			m.drain(pl, 0, true)
		}
		return PoolInfo{WarmPool: cfg}, nil
	}
	if err := m.Store.PutWarmPool(cfg); err != nil {
		return PoolInfo{}, err
	}
	pl := m.poolFor(cfg)
	m.drain(pl, size, false)
	m.refill(pl)
	return m.poolInfo(pl), nil
}

// poolFor returns the running pool for cfg, made when there is none, with cfg's size.
func (m *Manager) poolFor(cfg store.WarmPool) *pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pl := m.pools[cfg.ID()]
	if pl == nil {
		pl = &pool{cfg: cfg}
		m.pools[cfg.ID()] = pl
	}
	pl.cfg = cfg
	return pl
}

// drain deletes pl's ready clones beyond keep; gone also stops its refills.
func (m *Manager) drain(pl *pool, keep int, gone bool) {
	m.mu.Lock()
	var extra []Clone
	if len(pl.ready) > keep {
		extra = slices.Clone(pl.ready[keep:])
		pl.ready = pl.ready[:keep]
	}
	if gone {
		pl.gone = true
	}
	m.mu.Unlock()
	for _, c := range extra {
		m.dropPooled(c)
	}
}

func (m *Manager) dropPooled(c Clone) {
	if err := m.Engine.DeleteClone(context.Background(), c.ID()); err != nil {
		log.Printf("ephemeral: deleting pooled clone %s: %v", c.ID(), err)
	}
}

// Pools lists every pool with how many clones it has ready.
func (m *Manager) Pools() ([]PoolInfo, error) {
	cfgs, err := m.Store.ListWarmPools()
	if err != nil {
		return nil, err
	}
	out := make([]PoolInfo, 0, len(cfgs))
	for _, cfg := range cfgs {
		m.mu.Lock()
		pl := m.pools[cfg.ID()]
		m.mu.Unlock()
		if pl == nil {
			out = append(out, PoolInfo{WarmPool: cfg})
			continue
		}
		out = append(out, m.poolInfo(pl))
	}
	return out, nil
}

func (m *Manager) poolInfo(pl *pool) PoolInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return PoolInfo{WarmPool: pl.cfg, WarmKey: pl.key, Ready: len(pl.ready), Err: pl.err}
}

// StartPools fills every stored pool, once at anvild's start after Cleanup.
func (m *Manager) StartPools() {
	if m.Engine == nil {
		return
	}
	cfgs, err := m.Store.ListWarmPools()
	if err != nil {
		log.Printf("ephemeral: reading warm pools: %v", err)
		return
	}
	for _, cfg := range cfgs {
		m.refill(m.poolFor(cfg))
	}
}

// refill restores clones for pl in the background until it is full, building its warm
// snapshot first when needed. A failed refill tries again after poolRetry.
func (m *Manager) refill(pl *pool) {
	m.mu.Lock()
	if pl.filling || pl.gone {
		m.mu.Unlock()
		return
	}
	pl.filling = true
	m.mu.Unlock()
	go func() {
		err := m.fill(pl)
		m.mu.Lock()
		pl.filling, pl.err = false, ""
		if err != nil {
			pl.err = err.Error()
		}
		m.mu.Unlock()
		if err != nil {
			log.Printf("ephemeral: filling the %s pool: %v (trying again in %s)", pl.cfg.ID(), err, poolRetry)
			time.AfterFunc(poolRetry, func() { m.refill(pl) })
		}
	}()
}

func (m *Manager) fill(pl *pool) error {
	ctx := context.Background()
	for {
		m.mu.Lock()
		cfg, done := pl.cfg, pl.gone || len(pl.ready) >= pl.cfg.Size
		m.mu.Unlock()
		if done {
			return nil
		}
		_, spec, err := m.Warm(ctx, WarmParams{Distro: cfg.Distro, Arch: cfg.Arch, CPUs: cfg.CPUs, MemoryMiB: cfg.MemoryMiB}, false, nil)
		if err != nil {
			return err
		}
		m.mu.Lock()
		var stale []Clone
		if pl.key != spec.Key { // a new snapshot, after an upgrade: older clones go
			stale, pl.ready, pl.key = pl.ready, nil, spec.Key
		}
		m.mu.Unlock()
		for _, c := range stale {
			m.dropPooled(c)
		}
		id := "eph-P" + ulid.Make().String()
		clone, err := m.restoreFresh(ctx, m.Engine, id, spec, func(string) {}, false)
		if err != nil {
			return err
		}
		m.mu.Lock()
		keep := !pl.gone && pl.key == spec.Key && len(pl.ready) < pl.cfg.Size
		if keep {
			pl.ready = append(pl.ready, clone)
		}
		m.mu.Unlock()
		if !keep {
			m.dropPooled(clone)
		}
	}
}

// takePooled hands out a ready clone of warm snapshot key, nil when no pool has one. The
// clone's clock is set first, which also checks it still answers.
func (m *Manager) takePooled(ctx context.Context, key string) Clone {
	for {
		m.mu.Lock()
		var (
			pl *pool
			c  Clone
		)
		for _, p := range m.pools {
			if p.key == key && len(p.ready) > 0 {
				pl, c = p, p.ready[0]
				p.ready = p.ready[1:]
				break
			}
		}
		m.mu.Unlock()
		if c == nil {
			return nil
		}
		m.refill(pl)
		if err := m.wake(ctx, c); err != nil {
			log.Printf("ephemeral: pooled clone %s doesn't answer, deleting it: %v", c.ID(), err)
			m.dropPooled(c)
			continue
		}
		return c
	}
}

// evictPooled deletes one ready clone of any pool, to free its memory for a run.
func (m *Manager) evictPooled() bool {
	m.mu.Lock()
	var (
		pl *pool
		c  Clone
	)
	for _, p := range m.pools {
		if n := len(p.ready); n > 0 {
			pl, c = p, p.ready[n-1]
			p.ready = p.ready[:n-1]
			break
		}
	}
	m.mu.Unlock()
	if c == nil {
		return false
	}
	log.Printf("ephemeral: deleting pooled clone %s to free memory for a run", c.ID())
	m.dropPooled(c)
	// Later, so the run that needs the memory gets it first.
	time.AfterFunc(poolRetry, func() { m.refill(pl) })
	return true
}

// dropPoolsOf removes the pools of warm snapshot key, which is being deleted.
func (m *Manager) dropPoolsOf(key string) error {
	m.mu.Lock()
	var gone []*pool
	for id, p := range m.pools {
		if p.key == key {
			gone = append(gone, p)
			delete(m.pools, id)
		}
	}
	m.mu.Unlock()
	for _, p := range gone {
		if err := m.Store.DeleteWarmPool(p.cfg.ID()); err != nil {
			return err
		}
		m.drain(p, 0, true)
	}
	return nil
}

// setClock sets a pooled clone's clock as it is handed out.
func setClock(ctx context.Context, c Clone) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return fmt.Errorf("reaching anvil-guest: %w", err)
	}
	return guestexec.SetupClone(ctx, conn, time.Now(), "")
}
