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

	"golang.org/x/sys/unix"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// DiskHelper builds clone disks as root; hostdev.Client asks the root helper.
type DiskHelper interface {
	CreateCloneDisk(ctx context.Context, d hostdev.CloneDisk) error
	DeleteCloneDisk(ctx context.Context, id string) error
}

// How a clone got its disk, the name of its restore phase.
const (
	diskReflink = "disk reflink"
	diskDM      = "disk dm-snapshot"
	diskCopy    = "disk copy"
)

// cloneDisk puts clone id's disk in its jail as warmDisk: a reflink copy where the
// filesystem has them, else a dm-snapshot from the root helper, else a full copy.
func (b *FCBackend) cloneDisk(ctx context.Context, id, key, dir string, paths fcInstancePaths) (string, error) {
	src := filepath.Join(dir, warmDisk)
	disk := filepath.Join(instanceDir(id), warmDisk)
	err := reflinkFile(src, disk)
	if err == nil {
		_, err = jailFile(paths, disk, warmDisk, false)
		return diskReflink, err
	}
	_ = os.Remove(disk)
	if b.Disks != nil && paths.Jail != nil && !b.dmOff.Load() {
		if err := b.dmDisk(ctx, id, key, src, paths); err == nil {
			return diskDM, nil
		} else if b.dmOff.CompareAndSwap(false, true) {
			log.Printf("vm: clone disks fall back to full copies until anvild restarts: %v", err)
		}
	}
	if err := image.CloneDisk(ctx, src, disk, 0); err != nil {
		return "", err
	}
	_, err = jailFile(paths, disk, warmDisk, false)
	return diskCopy, err
}

// dmDisk asks the root helper for a dm-snapshot of src with a sparse COW file of its size.
func (b *FCBackend) dmDisk(ctx context.Context, id, key, src string, paths fcInstancePaths) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	cow := filepath.Join(instanceDir(id), hostdev.CloneCOWFile)
	f, err := os.OpenFile(cow, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = f.Truncate(info.Size())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = b.Disks.CreateCloneDisk(ctx, hostdev.CloneDisk{ID: id, Key: key, Jail: paths.root()})
	}
	if err != nil {
		_ = os.Remove(cow)
		return fmt.Errorf("vm: dm-snapshot disk for clone %s: %w", id, err)
	}
	return nil
}

// reflinkFile is reflink; tests replace it to pick the filesystem's behavior.
var reflinkFile = reflink

// reflink makes dst a copy-on-write clone of src, failing where the filesystem can't.
func reflink(src, dst string) error {
	return cloneOrCopy(src, dst, false)
}

// privateCopy makes dst a private copy of src: a reflink where the filesystem allows, else
// a plain copy, which the kernel does with copy_file_range.
func privateCopy(src, dst string) error {
	return cloneOrCopy(src, dst, true)
}

func cloneOrCopy(src, dst string, fallback bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err != nil && fallback {
		_, err = io.Copy(out, in)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("vm: reflink %s: %w", src, err)
	}
	return nil
}

// deleteCloneDisk removes clone id's dm-snapshot, if it has one.
func (b *FCBackend) deleteCloneDisk(ctx context.Context, id string) error {
	if b.Disks == nil {
		return nil
	}
	if err := b.Disks.DeleteCloneDisk(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("vm: removing the disk of clone %s: %w", id, err)
	}
	return nil
}
