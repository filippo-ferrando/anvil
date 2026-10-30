package guestexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Request is one command for Run. Upload, Stdin and Resize are optional.
type Request struct {
	Header
	Upload io.Reader // a tar stream, extracted into WorkDir in the guest
	Stdin  io.Reader
	Resize <-chan Size
	Stdout io.Writer
	Stderr io.Writer // unused with a TTY, whose output all arrives on Stdout
}

// Run runs r on the guest at the other end of conn and returns the command's exit code.
// Cancelling ctx closes conn, and the guest then kills the command.
func Run(ctx context.Context, conn io.ReadWriteCloser, r Request) (int, error) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	fw := &frameWriter{w: conn}
	hdr := r.Header
	hdr.Version = Version
	hdr.Upload = r.Upload != nil
	data, err := json.Marshal(hdr)
	if err != nil {
		return -1, err
	}
	if err := fw.write(frameHeader, data); err != nil {
		return -1, ctxErr(ctx, fmt.Errorf("guestexec: sending the command: %w", err))
	}
	if r.Upload != nil {
		if err := fw.copyFrames(frameUpload, r.Upload); err != nil {
			return -1, ctxErr(ctx, fmt.Errorf("guestexec: uploading: %w", err))
		}
		if err := fw.write(frameUploadEnd, nil); err != nil {
			return -1, ctxErr(ctx, fmt.Errorf("guestexec: uploading: %w", err))
		}
	}
	// A write that fails after the command ends only means the guest closed the connection.
	if r.Stdin != nil {
		go func() {
			if fw.copyFrames(frameStdin, r.Stdin) == nil {
				_ = fw.write(frameStdinClose, nil)
			}
		}()
	} else {
		go func() { _ = fw.write(frameStdinClose, nil) }() // a command reading stdin sees its end
	}
	if r.Resize != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case s := <-r.Resize:
					_ = fw.write(frameResize, encodeSize(s))
				case <-done:
					return
				}
			}
		}()
	}

	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = errors.New("the guest closed the connection before the command exited")
			}
			return -1, ctxErr(ctx, fmt.Errorf("guestexec: %w", err))
		}
		switch typ {
		case frameStdout:
			if r.Stdout != nil {
				_, _ = r.Stdout.Write(payload)
			}
		case frameStderr:
			if r.Stderr != nil {
				_, _ = r.Stderr.Write(payload)
			}
		case frameExit:
			if len(payload) != 4 {
				return -1, fmt.Errorf("guestexec: malformed exit frame")
			}
			return int(int32(binary.BigEndian.Uint32(payload))), nil
		default:
			return -1, fmt.Errorf("guestexec: unexpected frame type %d from the guest", typ)
		}
	}
}

// ctxErr prefers ctx's error, since a cancel shows up as a failed read or write on conn.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// SetupClone sets the guest's clock to now and, when not empty, its hostname: the first
// thing on a clone restored from a snapshot.
func SetupClone(ctx context.Context, conn io.ReadWriteCloser, now time.Time, hostname string) error {
	var stderr strings.Builder
	code, err := Run(ctx, conn, Request{Header: Header{SetClock: now.UnixNano(), Hostname: hostname}, Stderr: &stderr})
	if err == nil && code != 0 {
		err = fmt.Errorf("guestexec: %s", strings.TrimSpace(stderr.String()))
	}
	return err
}
