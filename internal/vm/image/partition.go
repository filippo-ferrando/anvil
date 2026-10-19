package image

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

const sectorSize = 512

// GPT partition type GUIDs, in their on-disk byte order.
var (
	gptRootX86_64   = guidBytes("4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709")
	gptRootAArch64  = guidBytes("B921B045-1DF0-41C3-AF44-4C6F280D3FAE")
	gptLinuxData    = guidBytes("0FC63DAF-8483-4772-8E79-3D69D8477DE4")
	errNoPartitions = errors.New("no partition table")
)

// RootPartUUID returns the PARTUUID of the root partition of the raw disk at path, as
// the kernel's root=PARTUUID= expects it. It prefers a partition typed as the arch's
// root, then the largest Linux data partition, then the largest partition.
func RootPartUUID(path, arch string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("image: %w", err)
	}
	defer f.Close()

	head := make([]byte, 2*sectorSize)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", fmt.Errorf("image: reading partition table of %s: %w", path, err)
	}
	if bytes.Equal(head[sectorSize:sectorSize+8], []byte("EFI PART")) {
		uuid, err := gptRoot(f, head[sectorSize:], arch)
		if err != nil {
			return "", fmt.Errorf("image: %s: %w", path, err)
		}
		return uuid, nil
	}
	uuid, err := mbrRoot(head[:sectorSize])
	if err != nil {
		return "", fmt.Errorf("image: %s: %w", path, err)
	}
	return uuid, nil
}

func gptRoot(f io.ReaderAt, header []byte, arch string) (string, error) {
	entriesLBA := binary.LittleEndian.Uint64(header[72:80])
	count := binary.LittleEndian.Uint32(header[80:84])
	entrySize := binary.LittleEndian.Uint32(header[84:88])
	if entrySize < 128 || count == 0 || count > 1024 {
		return "", fmt.Errorf("odd GPT header (%d entries of %d bytes)", count, entrySize)
	}
	table := make([]byte, int(count)*int(entrySize))
	if _, err := f.ReadAt(table, int64(entriesLBA)*sectorSize); err != nil {
		return "", fmt.Errorf("reading GPT entries: %w", err)
	}

	rootType := gptRootX86_64
	if arch == "aarch64" {
		rootType = gptRootAArch64
	}
	var root, data, largest []byte
	var dataSize, largestSize uint64
	for i := 0; i < int(count); i++ {
		e := table[i*int(entrySize) : (i+1)*int(entrySize)]
		typ := e[0:16]
		if bytes.Equal(typ, make([]byte, 16)) {
			continue // unused slot
		}
		size := binary.LittleEndian.Uint64(e[40:48]) - binary.LittleEndian.Uint64(e[32:40]) + 1
		switch {
		case bytes.Equal(typ, rootType) && root == nil:
			root = e[16:32]
		case bytes.Equal(typ, gptLinuxData) && size > dataSize:
			data, dataSize = e[16:32], size
		}
		if size > largestSize {
			largest, largestSize = e[16:32], size
		}
	}
	for _, guid := range [][]byte{root, data, largest} {
		if guid != nil {
			return formatGUID(guid), nil
		}
	}
	return "", errNoPartitions
}

// mbrRoot picks the largest Linux (0x83) partition, else the largest one, and names it
// the way the kernel does for MBR disks: "<disk signature>-<partition number>".
func mbrRoot(sector []byte) (string, error) {
	if sector[510] != 0x55 || sector[511] != 0xAA {
		return "", errNoPartitions
	}
	best, bestLinux := -1, false
	var bestSize uint32
	for i := 0; i < 4; i++ {
		e := sector[446+16*i : 446+16*(i+1)]
		typ, size := e[4], binary.LittleEndian.Uint32(e[12:16])
		if typ == 0 || size == 0 || typ == 0x05 || typ == 0x0f || typ == 0xee {
			continue // empty, extended or a GPT protective entry
		}
		linux := typ == 0x83
		if best == -1 || (linux && !bestLinux) || (linux == bestLinux && size > bestSize) {
			best, bestLinux, bestSize = i, linux, size
		}
	}
	if best == -1 {
		return "", errNoPartitions
	}
	sig := binary.LittleEndian.Uint32(sector[440:444])
	return fmt.Sprintf("%08x-%02d", sig, best+1), nil
}

// formatGUID renders an on-disk GUID: the first three fields are little-endian.
func formatGUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]), b[8:10], b[10:16])
}

// guidBytes is the inverse of formatGUID, for the type constants above.
func guidBytes(s string) []byte {
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		panic("image: bad GUID " + s)
	}
	slices.Reverse(raw[0:4])
	slices.Reverse(raw[4:6])
	slices.Reverse(raw[6:8])
	return raw
}
