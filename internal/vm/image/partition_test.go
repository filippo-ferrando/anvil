package image

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

type gptPart struct {
	typ, guid   string
	first, last uint64
}

// writeGPT writes a minimal GPT disk: the header at LBA 1, 4 entries of 128 bytes at LBA 2.
func writeGPT(t *testing.T, parts []gptPart) string {
	t.Helper()
	disk := make([]byte, 4*sectorSize)
	h := disk[sectorSize:]
	copy(h, "EFI PART")
	binary.LittleEndian.PutUint64(h[72:80], 2)
	binary.LittleEndian.PutUint32(h[80:84], 4)
	binary.LittleEndian.PutUint32(h[84:88], 128)
	for i, p := range parts {
		e := disk[2*sectorSize+i*128:]
		copy(e[0:16], guidBytes(p.typ))
		copy(e[16:32], guidBytes(p.guid))
		binary.LittleEndian.PutUint64(e[32:40], p.first)
		binary.LittleEndian.PutUint64(e[40:48], p.last)
	}
	path := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(path, disk, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	efiType  = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	swapType = "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F"
	dataType = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
)

func TestRootPartUUIDGPT(t *testing.T) {
	typed := writeGPT(t, []gptPart{
		{efiType, "11111111-1111-1111-1111-111111111111", 2048, 4095},
		{dataType, "22222222-2222-2222-2222-222222222222", 4096, 999999}, // larger, but not root-typed
		{"4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709", "33333333-3333-4333-8333-333333333333", 1000000, 1000999},
		{"B921B045-1DF0-41C3-AF44-4C6F280D3FAE", "44444444-4444-4444-8444-444444444444", 1001000, 1001999},
	})
	for arch, want := range map[string]string{
		"x86_64": "33333333-3333-4333-8333-333333333333", "aarch64": "44444444-4444-4444-8444-444444444444",
	} {
		if got, err := RootPartUUID(typed, arch); err != nil || got != want {
			t.Errorf("%s: RootPartUUID = %q, %v; want the arch's root-typed partition %s", arch, got, err, want)
		}
	}

	// No root type: the largest Linux data partition, even when a swap one is larger.
	data := writeGPT(t, []gptPart{
		{dataType, "aaaaaaaa-0000-4000-8000-000000000001", 2048, 9999},
		{swapType, "aaaaaaaa-0000-4000-8000-000000000002", 10000, 9999999},
		{dataType, "aaaaaaaa-0000-4000-8000-000000000003", 10000000, 10999999},
	})
	if got, err := RootPartUUID(data, "x86_64"); err != nil || got != "aaaaaaaa-0000-4000-8000-000000000003" {
		t.Errorf("data-only: RootPartUUID = %q, %v", got, err)
	}

	// Nothing Linux at all: the largest partition.
	other := writeGPT(t, []gptPart{
		{efiType, "bbbbbbbb-0000-4000-8000-000000000001", 2048, 4095},
		{swapType, "bbbbbbbb-0000-4000-8000-000000000002", 4096, 99999},
	})
	if got, err := RootPartUUID(other, "x86_64"); err != nil || got != "bbbbbbbb-0000-4000-8000-000000000002" {
		t.Errorf("largest: RootPartUUID = %q, %v", got, err)
	}
}

func TestRootPartUUIDMBR(t *testing.T) {
	disk := make([]byte, 2*sectorSize)
	binary.LittleEndian.PutUint32(disk[440:444], 0xdeadbeef)
	entry := func(i int, typ byte, size uint32) {
		e := disk[446+16*i:]
		e[4] = typ
		binary.LittleEndian.PutUint32(e[12:16], size)
	}
	entry(0, 0x0c, 900000) // FAT, the largest
	entry(1, 0x83, 1000)
	entry(2, 0x83, 5000) // the largest Linux one
	entry(3, 0x05, 9999999)
	disk[510], disk[511] = 0x55, 0xAA
	path := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(path, disk, 0o640); err != nil {
		t.Fatal(err)
	}
	if got, err := RootPartUUID(path, "x86_64"); err != nil || got != "deadbeef-03" {
		t.Fatalf("RootPartUUID = %q, %v; want deadbeef-03", got, err)
	}

	if err := os.WriteFile(path, make([]byte, 2*sectorSize), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := RootPartUUID(path, "x86_64"); err == nil {
		t.Fatal("a disk with no partition table was accepted")
	}
}
