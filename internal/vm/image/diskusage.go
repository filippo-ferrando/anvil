package image

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"syscall"
)

// qcow2Magic starts every qcow2 file; the virtual size is a big-endian uint64 at offset 24.
var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

// LiveDiskUsage is DiskUsage without spawning qemu-img, for frequent polls: the allocated
// size comes from stat, the virtual size from the qcow2 header (file size for a raw disk).
func LiveDiskUsage(path string) (usedBytes, totalBytes int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	usedBytes = fi.Size()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		usedBytes = int64(st.Blocks) * 512
	}

	var hdr [32]byte
	n, err := io.ReadFull(f, hdr[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return 0, 0, fmt.Errorf("image: reading %s: %w", path, err)
	}
	if n == len(hdr) && string(hdr[:4]) == string(qcow2Magic) {
		return usedBytes, int64(binary.BigEndian.Uint64(hdr[24:32])), nil
	}
	return usedBytes, fi.Size(), nil
}
