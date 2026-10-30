//go:build linux

package guestexec

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY opens a new pseudo-terminal pair. The ioctls go through SyscallConn, which
// keeps the master non-blocking so read deadlines still work on it.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening a terminal: %w", err)
	}
	var n int
	err = control(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		var err error
		n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		return err
	})
	if err == nil {
		slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("opening a terminal: %w", err)
	}
	return master, slave, nil
}

func setSize(f *os.File, s Size) error {
	return control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: s.Rows, Col: s.Cols})
	})
}

func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var fnErr error
	if err := rc.Control(func(fd uintptr) { fnErr = fn(int(fd)) }); err != nil {
		return err
	}
	return fnErr
}
