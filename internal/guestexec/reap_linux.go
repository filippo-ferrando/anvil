//go:build linux

package guestexec

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// BinaryPath is where anvil-guest is installed in a guest.
const BinaryPath = "/usr/local/bin/anvil-guest"

// ReapArgv runs command through `anvil-guest reap`, see Reap.
func ReapArgv(command []string) []string {
	return append([]string{BinaryPath, "reap", "--"}, command...)
}

// Reap runs argv and returns its exit code once it and every process it left behind have
// exited, e.g. an app its launcher started in the background and then exited.
func Reap(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "anvil-guest reap: no command")
		return 2
	}
	// Orphaned descendants become this process's children instead of init's.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "anvil-guest reap: %v\n", err)
		return 1
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "anvil-guest reap: %v\n", err)
		return 127
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	code := 0
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil { // ECHILD: nothing left
			return code
		}
		if pid == cmd.Process.Pid {
			code = waitCode(ws)
		}
	}
}

func waitCode(ws unix.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
