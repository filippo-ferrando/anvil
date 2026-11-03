//go:build linux

package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// maxConsoleLineBytes caps how much of console.log one tailed line is assumed
// to need, so a tail never reads the whole file.
const maxConsoleLineBytes = 512

// sendInChunks streams the rest of f to send without holding it all in memory.
func sendInChunks(f *os.File, send func([]byte) error) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if sendErr := send(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("vm: reading console log: %w", err)
		}
	}
}

// Logs streams spec's console.log, QEMU's capture of the guest's serial
// console. Returns cleanly with no output if the log doesn't exist yet.
func (b *Backend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Logs called with a nil VMSpec")
	}
	path := filepath.Join(instanceDir(spec.ID), "console.log")

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("vm: opening console log: %w", err)
	}
	defer f.Close()

	if tailLines > 0 {
		// console.log is never rotated, so a long-lived VM's can reach gigabytes.
		// Read back only enough to hold the wanted lines.
		limit := int64(tailLines) * maxConsoleLineBytes
		if fi, err := f.Stat(); err == nil && fi.Size() > limit {
			if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
				return fmt.Errorf("vm: seeking console log: %w", err)
			}
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("vm: reading console log: %w", err)
		}
		if data = tailLinesOf(data, tailLines); len(data) > 0 {
			if err := send(data); err != nil {
				return err
			}
		}
	} else if err := sendInChunks(f, send); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	// Keep reading from the current offset; followFile handles QEMU truncating it on restart.
	return followFile(ctx, f, path, send)
}

// followPoll is how often followFile looks for new data without inotify, and how often it
// checks ctx with it.
const followPoll = 250 * time.Millisecond

// followFile sends what is appended to f (opened on path) until ctx ends. inotify wakes it
// as soon as the file grows, and every wake sends all there is, not one buffer.
func followFile(ctx context.Context, f *os.File, path string, send func([]byte) error) error {
	wait := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(followPoll):
			return nil
		}
	}
	if fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK); err == nil {
		defer unix.Close(fd)
		if _, err := unix.InotifyAddWatch(fd, path, unix.IN_MODIFY); err == nil {
			events := make([]byte, 4096)
			wait = func() error {
				fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				for {
					if err := ctx.Err(); err != nil {
						return err
					}
					n, err := unix.Poll(fds, int(followPoll.Milliseconds()))
					if err != nil && err != unix.EINTR {
						return fmt.Errorf("vm: watching console log: %w", err)
					}
					if n > 0 {
						for { // drained, so the next poll waits for new writes
							if _, err := unix.Read(fd, events); err != nil {
								break
							}
						}
						return nil
					}
				}
			}
		}
	}
	for {
		if err := rewindIfTruncated(f); err != nil {
			return err
		}
		if err := sendInChunks(f, send); err != nil {
			return err
		}
		if err := wait(); err != nil {
			return err
		}
	}
}

// rewindIfTruncated seeks f back to the start when the file got shorter than the
// current offset, as QEMU truncates console.log on every start.
func rewindIfTruncated(f *os.File) error {
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("vm: reading console log offset: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("vm: checking console log: %w", err)
	}
	if fi.Size() < off {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("vm: rewinding console log: %w", err)
		}
	}
	return nil
}

func tailLinesOf(data []byte, n int) []byte {
	// A log almost always ends in a newline, whose trailing empty field would
	// otherwise count as one of the n lines and return n-1 real ones.
	trailing := bytes.HasSuffix(data, []byte("\n"))
	body := bytes.TrimSuffix(data, []byte("\n"))
	lines := bytes.Split(body, []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := bytes.Join(lines, []byte("\n"))
	if trailing && len(out) > 0 {
		out = append(out, '\n')
	}
	return out
}
