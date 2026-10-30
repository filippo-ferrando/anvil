//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/proc"
)

// fcClone is a running clone. Clones aren't instances: they live only in anvild's memory.
type fcClone struct {
	id      string
	vsock   string
	started time.Time
	proc    *firecracker.Process
	paths   fcInstancePaths
}

func (c *fcClone) ID() string           { return c.id }
func (c *fcClone) StartedAt() time.Time { return c.started }
func (c *fcClone) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	return firecracker.DialVsock(ctx, c.vsock, guestexec.Port)
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
	cn, err := b.Clones.Setup(ctx, id)
	if err != nil {
		return nil, err
	}
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
	// The clone writes its own copy of the disk; everything else is shared read-only.
	disk := filepath.Join(instanceDir(id), warmDisk)
	if err := image.CloneDisk(ctx, filepath.Join(dir, warmDisk), disk, 0); err != nil {
		return nil, err
	}
	files := map[string]string{warmDisk: disk}
	for _, name := range []string{warmMem, warmSeed, warmGuest} {
		files[name] = filepath.Join(dir, name)
	}
	for name, src := range files {
		if _, err := jailFile(paths, src, name, false); err != nil {
			return nil, err
		}
	}
	state, err := jailFile(paths, filepath.Join(dir, warmState), warmState, true)
	if err != nil {
		return nil, err
	}

	proc, err := firecracker.Spawn(ctx, paths.Paths)
	if err != nil {
		return nil, fmt.Errorf("vm: spawning the clone's firecracker: %w", err)
	}
	c := &fcClone{id: id, vsock: paths.vsock, started: time.Now(), proc: proc, paths: paths}
	b.trackClone(c)
	if err := proc.WaitAPI(ctx); err != nil {
		return nil, fmt.Errorf("vm: %w", err)
	}
	if err := proc.API.LoadSnapshot(ctx, firecracker.SnapshotLoad{
		SnapshotPath: state,
		MemBackend:   firecracker.MemoryBackend{BackendType: "File", BackendPath: paths.InVMM(filepath.Join(paths.root(), warmMem))},
		ResumeVM:     true,
	}); err != nil {
		return nil, fmt.Errorf("vm: restoring the clone: %w: %s", err, logTailOf(paths.LogFile))
	}
	// The guest clock is still at the snapshot's time, which breaks TLS among others.
	conn, err := dialGuest(ctx, c.vsock, 10*time.Second)
	if err == nil {
		err = guestexec.SetClock(ctx, conn, time.Now())
	}
	if err != nil {
		return nil, fmt.Errorf("vm: setting the clone's clock through anvil-guest: %w", err)
	}
	return c, nil
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
		removeJail(c.paths)
	}
	b.removeLeftovers(id)
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
