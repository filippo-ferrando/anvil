// Package guestexec runs one command per connection inside a guest (over vsock in a
// Firecracker VM): the frame protocol, anvil-guest's server and the host's client.
package guestexec

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Port is the guest vsock port anvil-guest listens on (qemu-ga is on 1024).
const Port = 1025

// Version is the protocol version a Header carries.
const Version = 1

// Frame types. The host sends a header, the upload if any, then stdin and resizes; the
// guest sends output and finally the exit code.
const (
	frameHeader     byte = 1 // host: Header as JSON
	frameUpload     byte = 2 // host: a chunk of the upload's tar stream
	frameUploadEnd  byte = 3 // host: the upload is complete
	frameStdin      byte = 4 // host: input for the command
	frameStdinClose byte = 5 // host: no more input
	frameResize     byte = 6 // host: new terminal size, rows and cols as uint16
	frameStdout     byte = 7 // guest: output
	frameStderr     byte = 8 // guest: error output
	frameExit       byte = 9 // guest: the exit code as int32, the last frame
)

// maxFrame bounds one frame's payload, so a broken peer can't make the other allocate much.
const maxFrame = 1 << 20

// chunkSize is how much one data frame carries at most.
const chunkSize = 32 << 10

// Header describes the command to run. Commands run as anvil-guest's user (root).
type Header struct {
	Version int
	Argv    []string
	Env     []string // KEY=value, on top of a minimal default environment
	Cwd     string   // WorkDir when empty and there is an upload, / otherwise
	TTY     bool     // run on a terminal; stdout and stderr then arrive merged as stdout
	Rows    uint16
	Cols    uint16
	Upload  bool // a tar stream follows, extracted into WorkDir before the command starts
	// SetClock, in Unix nanoseconds, sets the guest clock before the command runs, which
	// may then be empty. A restored snapshot's clock is still at the snapshot's time.
	SetClock int64
}

// Size is a terminal size.
type Size struct{ Rows, Cols uint16 }

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// write sends one frame: a type byte, a big-endian uint32 length, then the payload.
func (fw *frameWriter) write(typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("guestexec: frame of %d bytes is too large", len(payload))
	}
	// One Write per frame, so concurrent writers never interleave inside a frame.
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	_, err := fw.w.Write(buf)
	return err
}

// copyFrames sends everything read from r as frames of typ.
func (fw *frameWriter) copyFrames(typ byte, r io.Reader) error {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := fw.write(typ, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("guestexec: frame of %d bytes is too large", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func encodeSize(s Size) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[:2], s.Rows)
	binary.BigEndian.PutUint16(b[2:], s.Cols)
	return b[:]
}

func decodeSize(b []byte) (Size, bool) {
	if len(b) != 4 {
		return Size{}, false
	}
	return Size{binary.BigEndian.Uint16(b[:2]), binary.BigEndian.Uint16(b[2:])}, true
}
