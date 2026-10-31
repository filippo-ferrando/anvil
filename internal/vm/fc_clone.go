//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/proc"
)

// fcClone is a running clone. Clones aren't instances: they live only in anvild's memory.
type fcClone struct {
	id      string
	vsock   string
	started time.Time
	proc    *firecracker.Process
	paths   fcInstancePaths
	phases  []ephemeral.Phase
	dmDisk  bool // its disk is a dm-snapshot from the root helper

	pages          *firecracker.PageHandler // with ANVIL_EPH_MEMORY=uffd
	saveWorkingSet func()                   // set on the clone that records the working set
}

func (c *fcClone) ID() string           { return c.id }
func (c *fcClone) StartedAt() time.Time { return c.started }
func (c *fcClone) RestorePhases() []ephemeral.Phase {
	return c.phases
}
func (c *fcClone) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	return firecracker.DialVsock(ctx, c.vsock, guestexec.Port)
}

// Listen listens where firecracker forwards the guest's connections to host port port:
// the vsock socket's path with "_<port>" appended.
func (c *fcClone) Listen(port uint32) (net.Listener, error) {
	path := c.vsock + "_" + strconv.FormatUint(uint64(port), 10)
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

func (b *FCBackend) RestoreClone(ctx context.Context, id string, s ephemeral.WarmSpec, dir string) (_ ephemeral.Clone, err error) {
	if err := b.canClone(); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(id, "eph-") {
		return nil, fmt.Errorf("vm: clone id %q must start with eph-", id)
	}
	defer func() {
		if err != nil {
			_ = b.DeleteClone(context.WithoutCancel(ctx), id)
		}
	}()
	phases := phaseClock{last: time.Now()}
	cn, err := b.Clones.Setup(ctx, id)
	if err != nil {
		return nil, err
	}
	phases.done("network")
	paths, err := b.clonePaths(id, cn.NetNS)
	if err != nil {
		return nil, err
	}
	paths.ConfigFile = "" // restored from the snapshot, not booted
	if err := os.MkdirAll(instanceDir(id), 0o750); err != nil {
		return nil, err
	}
	if err := prepareJail(id, paths); err != nil {
		return nil, err
	}
	// The clone writes its own disk; everything else is shared read-only.
	how, err := b.cloneDisk(ctx, id, s.Key, dir, paths)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{warmMem, warmSeed, warmGuest} {
		if _, err := jailFile(paths, filepath.Join(dir, name), name, false); err != nil {
			return nil, err
		}
	}
	state, err := jailFile(paths, filepath.Join(dir, warmState), warmState, true)
	if err != nil {
		return nil, err
	}
	phases.done(how)

	proc, err := firecracker.Spawn(ctx, paths.Paths)
	if err != nil {
		return nil, fmt.Errorf("vm: spawning the clone's firecracker: %w", err)
	}
	c := &fcClone{id: id, vsock: paths.vsock, started: time.Now(), proc: proc, paths: paths, dmDisk: how == diskDM}
	b.trackClone(c)
	if err := proc.WaitAPI(ctx); err != nil {
		return nil, fmt.Errorf("vm: %w", err)
	}
	phases.done("spawn")
	mem, err := b.cloneMemory(c, dir)
	if err != nil {
		return nil, err
	}
	if err := proc.API.LoadSnapshot(ctx, firecracker.SnapshotLoad{SnapshotPath: state, MemBackend: mem, ResumeVM: true}); err != nil {
		return nil, fmt.Errorf("vm: restoring the clone: %w: %s", err, logTailOf(paths.LogFile))
	}
	phases.done("load")
	if c.pages != nil {
		select {
		case <-c.pages.Ready():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := c.pages.Err(); err != nil {
			return nil, fmt.Errorf("vm: serving the clone's memory: %w", err)
		}
		n, _ := c.pages.Prefetched()
		phases.done(fmt.Sprintf("prefetch %d pages", n))
	}
	// The guest clock is still at the snapshot's time, which breaks TLS among others.
	conn, err := dialGuest(ctx, c.vsock, 10*time.Second)
	if err == nil {
		err = guestexec.SetupClone(ctx, conn, time.Now(), strings.ToLower(id))
	}
	if err != nil {
		return nil, fmt.Errorf("vm: setting the clone's clock and hostname through anvil-guest: %w", err)
	}
	phases.done("setup")
	c.phases = phases.list
	return c, nil
}

// phaseClock records restore phases, each one ending where the previous one did.
type phaseClock struct {
	last time.Time
	list []ephemeral.Phase
}

func (p *phaseClock) done(name string) {
	now := time.Now()
	p.list = append(p.list, ephemeral.Phase{Name: name, Took: now.Sub(p.last)})
	p.last = now
}

func (b *FCBackend) trackClone(c *fcClone) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clones == nil {
		b.clones = map[string]*fcClone{}
	}
	b.clones[c.id] = c
}

// DeleteClone kills clone id and removes its jail, netns and disk. A missing one is fine.
func (b *FCBackend) DeleteClone(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, "eph-") {
		return fmt.Errorf("vm: clone id %q must start with eph-", id)
	}
	b.mu.Lock()
	c := b.clones[id]
	delete(b.clones, id)
	b.mu.Unlock()
	var errs []error
	if c != nil {
		if err := c.proc.Kill("firecracker"); err != nil {
			errs = append(errs, err)
		}
		_ = c.proc.Close()
		if c.pages != nil {
			if c.saveWorkingSet != nil {
				c.saveWorkingSet()
			}
			_ = c.pages.Close()
		}
		removeJail(c.paths)
	}
	// A leftover clone (c nil) may have one too.
	if c == nil || c.dmDisk {
		if err := b.deleteCloneDisk(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	removeCloneLeftovers(b.ChrootBase, id)
	if err := b.Clones.Teardown(ctx, id); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(instanceDir(id)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// SweepClones removes what clones and warm builds of an earlier anvild left: their
// firecracker processes, jails, instance dirs and netns.
func (b *FCBackend) SweepClones(ctx context.Context) error {
	ids := map[string]bool{}
	for _, pattern := range []string{filepath.Join(b.ChrootBase, "*", "eph-*"), instanceDir("eph-*")} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			ids[filepath.Base(m)] = true
		}
	}
	b.mu.Lock()
	for id := range b.clones {
		delete(ids, id)
	}
	b.mu.Unlock()
	var errs []error
	for id := range ids {
		log.Printf("vm: removing leftover clone %s", id)
		killJailed(id)
		if err := b.DeleteClone(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if err := b.Clones.Sweep(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// logTailOf returns the last lines of a log file on one line, for an error message.
func logTailOf(path string) string {
	data, _ := os.ReadFile(path)
	return strings.Join(strings.Fields(string(tailLinesOf(data, 3))), " ")
}

// killJailed kills a firecracker left running in jail id by an earlier anvild.
func killJailed(id string) {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		args := proc.Args(pid)
		if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), firecracker.Binary) {
			continue
		}
		for i, a := range args {
			if (a == "--id" && i+1 < len(args) && args[i+1] == id) || a == "--id="+id {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}
