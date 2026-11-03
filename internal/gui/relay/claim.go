//go:build linux

package relay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// maxClaims bounds how many sessions can share one directory.
const maxClaims = 64

// Claim reserves the lowest n for which dir/<prefix><n> is free, holding an flock on
// dir/.<prefix><n>-lock so a crashed session never keeps it. release frees it again.
func Claim(dir, prefix string) (n int, release func(), err error) {
	for n = 0; n < maxClaims; n++ {
		f, err := lockSlot(filepath.Join(dir, "."+prefix+strconv.Itoa(n)+"-lock"))
		if err != nil {
			return 0, nil, err
		}
		if f == nil {
			continue
		}
		release = func() {
			os.Remove(f.Name())
			f.Close()
		}
		return n, release, nil
	}
	return 0, nil, fmt.Errorf("relay: more than %d sessions in %s", maxClaims, dir)
}

// lockSlot takes an flock on lockPath, or returns nil when another session holds it.
// It retries when the file was unlinked or swapped before the lock was taken.
func lockSlot(lockPath string) (*os.File, error) {
	for {
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o666)
		if err != nil {
			return nil, fmt.Errorf("relay: claiming %s: %w", lockPath, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, nil
			}
			return nil, fmt.Errorf("relay: locking %s: %w", lockPath, err)
		}
		held, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("relay: locking %s: %w", lockPath, err)
		}
		if cur, err := os.Lstat(lockPath); err == nil && os.SameFile(held, cur) {
			return f, nil
		}
		f.Close()
	}
}
