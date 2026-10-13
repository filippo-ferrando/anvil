package relay

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Xauthority address families used here.
const (
	familyLocal uint16 = 256
	familyWild  uint16 = 65535
)

// XAuthEntry is one record of an .Xauthority file.
type XAuthEntry struct {
	Family  uint16
	Address []byte
	Number  string
	Name    string
	Data    []byte
}

// XAuthorityPath returns $XAUTHORITY, or ~/.Xauthority when unset.
func XAuthorityPath() string {
	if p := os.Getenv("XAUTHORITY"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".Xauthority")
}

// ReadXAuthority parses every record in r.
func ReadXAuthority(r io.Reader) ([]XAuthEntry, error) {
	br := bufio.NewReader(r)
	var out []XAuthEntry
	for {
		var e XAuthEntry
		if err := binary.Read(br, binary.BigEndian, &e.Family); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		var fields [4][]byte
		for i := range fields {
			var n uint16
			if err := binary.Read(br, binary.BigEndian, &n); err != nil {
				return nil, fmt.Errorf("xauth: truncated record: %w", err)
			}
			fields[i] = make([]byte, n)
			if _, err := io.ReadFull(br, fields[i]); err != nil {
				return nil, fmt.Errorf("xauth: truncated record: %w", err)
			}
		}
		e.Address, e.Number, e.Name, e.Data = fields[0], string(fields[1]), string(fields[2]), fields[3]
		out = append(out, e)
	}
}

// WriteXAuthority writes entries in .Xauthority format.
func WriteXAuthority(w io.Writer, entries []XAuthEntry) error {
	for _, e := range entries {
		if err := binary.Write(w, binary.BigEndian, e.Family); err != nil {
			return err
		}
		for _, f := range [][]byte{e.Address, []byte(e.Number), []byte(e.Name), e.Data} {
			if err := binary.Write(w, binary.BigEndian, uint16(len(f))); err != nil {
				return err
			}
			if _, err := w.Write(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// CookieFor returns the first local or wildcard cookie for display number n.
func CookieFor(entries []XAuthEntry, n int) (XAuthEntry, bool) {
	want := strconv.Itoa(n)
	for _, e := range entries {
		if e.Number == want && (e.Family == familyLocal || e.Family == familyWild) {
			return e, true
		}
	}
	return XAuthEntry{}, false
}

// WriteGuestXAuthority writes the host cookie for display hostDisplay to dst as a wildcard
// entry for display guestDisplay, so it matches whatever hostname the instance has.
// It reports false (and writes nothing) when the host has no cookie for that display.
func WriteGuestXAuthority(src string, hostDisplay int, dst string, guestDisplay int) (bool, error) {
	f, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	entries, err := ReadXAuthority(f)
	if err != nil {
		return false, fmt.Errorf("xauth: reading %s: %w", src, err)
	}
	e, ok := CookieFor(entries, hostDisplay)
	if !ok {
		return false, nil
	}
	e.Family, e.Address, e.Number = familyWild, nil, strconv.Itoa(guestDisplay)

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	if err := WriteXAuthority(out, []XAuthEntry{e}); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}
