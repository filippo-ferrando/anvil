//go:build linux

package guestexec

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// WorkDir is where an upload is extracted, and the command's default directory then.
var WorkDir = "/work"

// drainTimeout bounds how long output is still read once the command and its process
// group are gone, for a daemon that kept the output open.
const drainTimeout = 2 * time.Second

var defaultEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"LANG=C.UTF-8",
}

// Serve runs the one command conn carries and closes conn once it's done. A closed
// connection before the command exits kills the command's process group.
func Serve(conn io.ReadWriteCloser) error {
	defer conn.Close()
	fw := &frameWriter{w: conn}
	typ, payload, err := readFrame(conn)
	if err != nil {
		return err
	}
	var hdr Header
	if typ != frameHeader || json.Unmarshal(payload, &hdr) != nil {
		return errors.New("guestexec: the connection didn't start with a header")
	}
	if hdr.Version != Version {
		return fail(fw, hdr.TTY, 2, "anvil-guest: protocol version %d, this guest speaks %d", hdr.Version, Version)
	}
	if hdr.SetClock != 0 {
		ts := unix.NsecToTimespec(hdr.SetClock)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			return fail(fw, hdr.TTY, 1, "anvil-guest: setting the clock: %v", err)
		}
	}
	if hdr.Hostname != "" {
		if err := setHostname(hdr.Hostname); err != nil {
			return fail(fw, hdr.TTY, 1, "anvil-guest: setting the hostname: %v", err)
		}
	}
	if (hdr.SetClock != 0 || hdr.Hostname != "") && len(hdr.Argv) == 0 {
		return sendExit(fw, 0)
	}
	if len(hdr.Argv) == 0 {
		return fail(fw, hdr.TTY, 2, "anvil-guest: no command")
	}
	if hdr.Upload {
		if err := receiveUpload(conn); err != nil {
			return fail(fw, hdr.TTY, 1, "anvil-guest: upload: %v", err)
		}
	}
	cwd := hdr.Cwd
	if cwd == "" {
		cwd = "/"
		if hdr.Upload {
			cwd = WorkDir
		}
	}

	r := &run{conn: conn, fw: fw, tty: hdr.TTY}
	cmd := exec.Command(hdr.Argv[0], hdr.Argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(append([]string(nil), defaultEnv...), hdr.Env...)
	if err := r.setupIO(cmd, hdr); err != nil {
		return fail(fw, hdr.TTY, 1, "anvil-guest: %v", err)
	}
	if hdr.User != "" {
		if err := runAs(cmd, hdr.User, hdr.Cwd == ""); err != nil {
			r.closeAll()
			return fail(fw, hdr.TTY, 1, "anvil-guest: %v", err)
		}
	}
	if err := cmd.Start(); err != nil {
		r.closeAll()
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			code = 127
		}
		return fail(fw, hdr.TTY, code, "anvil-guest: %v", err)
	}
	r.started(cmd.Process.Pid)
	return r.finish(cmd)
}

// runAs makes cmd run as name with its groups and environment, in its home when home is set.
func runAs(cmd *exec.Cmd, name string, home bool) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("user %s: uid %q: %w", name, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("user %s: gid %q: %w", name, u.Gid, err)
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	ids, _ := u.GroupIds()
	for _, id := range ids {
		if g, err := strconv.ParseUint(id, 10, 32); err == nil {
			cred.Groups = append(cred.Groups, uint32(g))
		}
	}
	cmd.SysProcAttr.Credential = cred
	if home {
		cmd.Dir = u.HomeDir
	}
	// Later entries win, so these replace the defaults' HOME.
	cmd.Env = append(cmd.Env, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	return nil
}

// run is one started command and the files that connect it to the connection.
type run struct {
	conn io.Reader
	fw   *frameWriter
	tty  bool

	stdin  *os.File   // the command's input: a pipe's write end, or the terminal
	output []*os.File // read ends: stdout and stderr, or the terminal
	child  []*os.File // the command's own ends, closed once it started

	mu     sync.Mutex
	pid    int
	exited bool
}

func (r *run) setupIO(cmd *exec.Cmd, hdr Header) error {
	if r.tty {
		master, slave, err := openPTY()
		if err != nil {
			return err
		}
		size := Size{Rows: hdr.Rows, Cols: hdr.Cols}
		if size.Rows == 0 || size.Cols == 0 {
			size = Size{Rows: 24, Cols: 80}
		}
		_ = setSize(master, size)
		r.stdin, r.output, r.child = master, []*os.File{master}, []*os.File{slave}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		return nil
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return err
	}
	r.stdin, r.output, r.child = inW, []*os.File{outR, errR}, []*os.File{inR, outW, errW}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func (r *run) closeAll() {
	for _, f := range append(append(r.child, r.output...), r.stdin) {
		if f != nil {
			f.Close()
		}
	}
}

func (r *run) started(pid int) {
	r.mu.Lock()
	r.pid = pid
	r.mu.Unlock()
	for _, f := range r.child {
		f.Close()
	}
	go r.readInput()
}

// killGroup kills what is left of the command's process group, at most until it was reaped.
func (r *run) killGroup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pid > 0 && !r.exited {
		_ = syscall.Kill(-r.pid, syscall.SIGKILL)
	}
}

// readInput feeds stdin and resizes to the command; the connection closing kills it.
func (r *run) readInput() {
	for {
		typ, payload, err := readFrame(r.conn)
		if err != nil {
			r.killGroup()
			return
		}
		switch typ {
		case frameStdin:
			_, _ = r.stdin.Write(payload)
		case frameStdinClose:
			if !r.tty {
				r.stdin.Close()
			}
		case frameResize:
			if s, ok := decodeSize(payload); ok && r.tty {
				_ = setSize(r.stdin, s)
			}
		}
	}
}

// finish waits for the command, kills its leftover background processes so the output
// ends, sends the rest of the output and then the exit code.
func (r *run) finish(cmd *exec.Cmd) error {
	var wg sync.WaitGroup
	types := []byte{frameStdout, frameStderr}
	for i, f := range r.output {
		wg.Go(func() {
			// A terminal reports EIO once its last writer is gone: that is its end.
			_ = r.fw.copyFrames(types[i], f)
		})
	}
	_ = cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the group outlives its leader
	r.mu.Lock()
	r.exited = true
	r.mu.Unlock()
	for _, f := range r.output {
		_ = f.SetReadDeadline(time.Now().Add(drainTimeout))
	}
	wg.Wait()
	r.closeAll()
	return sendExit(r.fw, exitCode(cmd.ProcessState))
}

// exitCode is the shell's convention: the exit status, or 128 plus the killing signal.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// fail reports an error the way a shell would, on the command's error output, then exits.
func fail(fw *frameWriter, tty bool, code int, format string, args ...any) error {
	typ := frameStderr
	if tty {
		typ = frameStdout
	}
	_ = fw.write(typ, []byte(fmt.Sprintf(format, args...)+"\n"))
	return sendExit(fw, code)
}

func sendExit(fw *frameWriter, code int) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return fw.write(frameExit, b[:])
}

// receiveUpload extracts the upload frames into WorkDir. After a failed extraction the
// rest of the upload is still read, so the error reaches the host instead of a closed conn.
func receiveUpload(conn io.Reader) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := extract(pr, WorkDir)
		_, _ = io.Copy(io.Discard, pr)
		done <- err
	}()
	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			pw.CloseWithError(err)
			<-done
			return err
		}
		switch typ {
		case frameUpload:
			_, _ = pw.Write(payload)
		case frameUploadEnd:
			pw.Close()
			return <-done
		default:
			pw.CloseWithError(errors.New("upload interrupted"))
			<-done
			return fmt.Errorf("unexpected frame type %d during the upload", typ)
		}
	}
}
