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
		lockPath := filepath.Join(dir, "."+prefix+strconv.Itoa(n)+"-lock")
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o666)
		if err != nil {
			return 0, nil, fmt.Errorf("relay: claiming %s: %w", lockPath, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			return 0, nil, fmt.Errorf("relay: locking %s: %w", lockPath, err)
		}
		release = func() {
			os.Remove(lockPath)
			f.Close()
		}
		return n, release, nil
	}
	return 0, nil, fmt.Errorf("relay: more than %d sessions in %s", maxClaims, dir)
}
