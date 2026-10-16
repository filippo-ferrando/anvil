package hostpath

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Resolve returns p with every symlink resolved, and the uid owning that directory.
func Resolve(p string) (string, uint32, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", 0, err
	}
	if !info.IsDir() {
		return "", 0, fmt.Errorf("%s is not a directory", p)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, fmt.Errorf("%s: owner unknown", p)
	}
	return real, st.Uid, nil
}

// CheckResolved fails when p, stored already resolved, now leads somewhere else: a
// directory on the way was swapped for a symlink since its owner was checked.
func CheckResolved(p string) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	if real != filepath.Clean(p) {
		return fmt.Errorf("%s now resolves to %s, refusing it: only the path checked when it was added is trusted", p, real)
	}
	return nil
}
