//go:build linux

package firecracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	uffdUserModeOnly    = 1      // UFFD_USER_MODE_ONLY: allowed without privileges
	uffdFeatureEvRemove = 1 << 3 // UFFD_FEATURE_EVENT_REMOVE
)

// fakeVMM plays firecracker's side: anonymous guest memory registered with a new uffd,
// handed to the handler through the same handshake.
type fakeVMM struct {
	uffd int
	mem  []byte
}

func newFakeVMM(t *testing.T, size int) *fakeVMM {
	t.Helper()
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly, 0, 0)
	if errno != 0 {
		t.Skipf("no userfaultfd here: %v", errno)
	}
	api := [3]uint64{0xAA, uffdFeatureEvRemove, 0}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioAPI, uintptr(unsafe.Pointer(&api))); errno != 0 {
		unix.Close(int(fd))
		t.Skipf("UFFDIO_API with EVENT_REMOVE: %v", errno)
	}
	mem, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	reg := [4]uint64{uint64(uintptr(unsafe.Pointer(&mem[0]))), uint64(size), 1 /* MODE_MISSING */, 0}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioRegister, uintptr(unsafe.Pointer(&reg))); errno != 0 {
		t.Fatalf("UFFDIO_REGISTER: %v", errno)
	}
	v := &fakeVMM{uffd: int(fd), mem: mem}
	t.Cleanup(func() {
		unix.Munmap(mem)
		unix.Close(v.uffd)
	})
	return v
}

// handshake connects to sock and sends the regions with the uffd, as firecracker does.
func (v *fakeVMM) handshake(t *testing.T, sock string, maps []UffdMapping) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body, _ := json.Marshal(maps)
	if _, _, err := conn.(*net.UnixConn).WriteMsgUnix(body, unix.UnixRights(v.uffd), nil); err != nil {
		t.Fatal(err)
	}
}

// read reads guest memory on its own locked thread: a fault blocks it until it is served.
func read(mem []byte, off, n int) []byte {
	out := make(chan []byte)
	go func() {
		runtime.LockOSThread()
		out <- slices.Clone(mem[off : off+n])
	}()
	select {
	case b := <-out:
		return b
	case <-time.After(5 * time.Second):
		return nil
	}
}

func TestPageHandler(t *testing.T) {
	ps := os.Getpagesize()
	const pages = 16
	vmm := newFakeVMM(t, pages*ps)
	dir := t.TempDir()
	memFile := filepath.Join(dir, "mem")
	content := make([]byte, pages*ps)
	for p := range pages {
		copy(content[p*ps:], bytes.Repeat([]byte{byte('a' + p)}, ps))
	}
	if err := os.WriteFile(memFile, content, 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := ListenPages(filepath.Join(dir, "uffd.sock"), memFile, []uint32{2, 3, 4, 9}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	base := uint64(uintptr(unsafe.Pointer(&vmm.mem[0])))
	vmm.handshake(t, filepath.Join(dir, "uffd.sock"), []UffdMapping{{BaseHostVirtAddr: base, Size: uint64(pages * ps), PageSize: uint64(ps)}})
	select {
	case <-h.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never got ready")
	}
	if h.Err() != nil {
		t.Fatal(h.Err())
	}
	if n, _ := h.Prefetched(); n != 4 {
		t.Errorf("prefetched %d pages, want 4", n)
	}

	// A prefetched page and two faulting ones read the file's contents.
	for _, p := range []int{3, 7, 12} {
		if got := read(vmm.mem, p*ps+10, 3); string(got) != string(bytes.Repeat([]byte{byte('a' + p)}, 3)) {
			t.Fatalf("page %d reads %q", p, got)
		}
	}
	if ws := h.WorkingSet(); !slices.Equal(ws, []uint32{7, 12}) {
		t.Errorf("working set %v, want the faulted pages 7 and 12", ws)
	}

	// The balloon giving pages back: they read as zeros afterwards.
	if err := unix.Madvise(vmm.mem[7*ps:8*ps], unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // for the handler to unregister the range
	if got := read(vmm.mem, 7*ps, 3); !bytes.Equal(got, []byte{0, 0, 0}) {
		t.Errorf("a removed page reads %q, want zeros", got)
	}
}

func TestPageHandlerBadHandshake(t *testing.T) {
	dir := t.TempDir()
	h, err := ListenPages(filepath.Join(dir, "uffd.sock"), filepath.Join(dir, "mem"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, "uffd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte(`[]`)) // no fd
	conn.Close()
	<-h.Ready()
	if h.Err() == nil {
		t.Error("a handshake without a uffd was accepted")
	}
	h.Close()

	for _, bad := range []string{`[]`, `nope`, `[{"base_host_virt_addr":0,"size":4096,"offset":0,"page_size":3000}]`} {
		if _, err := parseUffdMappings([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	maps, err := parseUffdMappings([]byte(`[{"base_host_virt_addr":4096,"size":8192,"offset":0,"page_size":4096,"page_size_kib":4096}]`))
	if err != nil || maps[0].Size != 8192 {
		t.Errorf("firecracker's handshake = %+v, %v", maps, err)
	}
}

func TestWorkingSetFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workingset")
	if got, err := ReadWorkingSet(path); err != nil || got != nil {
		t.Fatalf("missing file = %v, %v", got, err)
	}
	pages := []uint32{0, 1, 2, 7, 9, 10, 4000}
	if err := WriteWorkingSet(path, pages); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() != 4*8 {
		t.Errorf("file holds %d bytes, want 4 runs of 8", info.Size())
	}
	if got, err := ReadWorkingSet(path); err != nil || !slices.Equal(got, pages) {
		t.Errorf("read back %v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("odd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorkingSet(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("a broken file read as %v", err)
	}
}
