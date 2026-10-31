//go:build linux

package firecracker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// userfaultfd ioctls and events (linux/userfaultfd.h), the same on x86_64 and arm64.
const (
	uffdioAPI        = 0xC018AA3F
	uffdioRegister   = 0xC020AA00
	uffdioUnregister = 0x8010AA01
	uffdioCopy       = 0xC028AA03

	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x15
	uffdMsgSize        = 32
)

// UffdMapping is one guest memory region, as firecracker sends them in the UFFD handshake.
type UffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"` // in the memory file
	PageSize         uint64 `json:"page_size"`
}

// PageHandler serves a restored VM's guest memory from its snapshot memory file through
// userfaultfd: firecracker's "Uffd" memory backend. Each page the guest touches is copied
// into its memory, so VMs no longer share the file's pages as with the "File" backend.
type PageHandler struct {
	ln       net.Listener
	memFile  string
	prefetch []uint32 // memory file pages to copy in first
	record   time.Duration

	stop      int // eventfd that ends serve
	ready     chan struct{}
	done      chan struct{}
	prefetchN int
	took      time.Duration
	err       error

	mu      sync.Mutex
	touched map[uint32]struct{} // pages that faulted while recording
	stopRec time.Time
}

// ListenPages listens on sock for firecracker's handshake and serves its VM's memory from
// memFile: prefetch pages first, then faults. With record, the pages that fault in that
// long after the handshake are kept for WorkingSet.
func ListenPages(sock, memFile string, prefetch []uint32, record time.Duration) (*PageHandler, error) {
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("firecracker: page handler: %w", err)
	}
	stop, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("firecracker: page handler: %w", err)
	}
	h := &PageHandler{ln: ln, memFile: memFile, prefetch: prefetch, record: record, stop: stop,
		ready: make(chan struct{}), done: make(chan struct{}), touched: map[uint32]struct{}{}}
	go h.run()
	return h, nil
}

// Ready is closed once the handshake is done and the prefetch copied, or the handler failed.
func (h *PageHandler) Ready() <-chan struct{} { return h.ready }

// Prefetched is how many pages the prefetch copied and how long it took, once Ready.
func (h *PageHandler) Prefetched() (int, time.Duration) { return h.prefetchN, h.took }

// Err is why the handler stopped early, once Ready or after Close.
func (h *PageHandler) Err() error { return h.err }

// WorkingSet is the pages that faulted while recording, sorted.
func (h *PageHandler) WorkingSet() []uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]uint32, 0, len(h.touched))
	for p := range h.touched {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Close stops serving, once firecracker is gone: a VM whose handler stops freezes on its next fault.
func (h *PageHandler) Close() error {
	_, _ = unix.Write(h.stop, binary.NativeEndian.AppendUint64(nil, 1))
	h.ln.Close()
	<-h.done
	unix.Close(h.stop)
	return nil
}

func (h *PageHandler) run() {
	defer close(h.done)
	readyOnce := sync.OnceFunc(func() { close(h.ready) })
	defer readyOnce()
	uffd, maps, err := h.handshake()
	if err != nil {
		h.err = err
		return
	}
	defer unix.Close(uffd)
	f, err := os.Open(h.memFile)
	if err != nil {
		h.err = err
		return
	}
	info, err := f.Stat()
	if err == nil && info.Size() == 0 {
		err = errors.New("empty memory file")
	}
	var mem []byte
	if err == nil {
		mem, err = unix.Mmap(int(f.Fd()), 0, int(info.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
	}
	f.Close()
	if err != nil {
		h.err = fmt.Errorf("firecracker: mapping %s: %w", h.memFile, err)
		return
	}
	defer unix.Munmap(mem)
	s := &pageServer{uffd: uffd, maps: maps, mem: mem, pageSize: maps[0].PageSize}

	start := time.Now()
	h.prefetchN = s.prefetch(h.prefetch)
	h.took = time.Since(start)
	h.mu.Lock()
	h.stopRec = time.Now().Add(h.record)
	h.mu.Unlock()
	readyOnce()
	if err := s.serve(h.stop, h.fault); err != nil {
		h.err = err
		log.Printf("firecracker: page handler for %s: %v", h.memFile, err)
	}
}

// fault records the file page of a fault while recording.
func (h *PageHandler) fault(page uint32) {
	h.mu.Lock()
	if time.Now().Before(h.stopRec) {
		h.touched[page] = struct{}{}
	}
	h.mu.Unlock()
}

// handshake takes firecracker's one connection: a JSON array of regions with the uffd
// attached (SCM_RIGHTS). Nothing else is sent on it.
func (h *PageHandler) handshake() (int, []UffdMapping, error) {
	conn, err := h.ln.Accept()
	if err != nil {
		return -1, nil, fmt.Errorf("firecracker: waiting for the uffd handshake: %w", err)
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return -1, nil, err
	}
	buf := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4))
	var n, oobn int
	var recvErr error
	if err := raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, recvErr = unix.Recvmsg(int(fd), buf, oob, 0)
		return !errors.Is(recvErr, unix.EAGAIN)
	}); err != nil {
		return -1, nil, err
	}
	if recvErr != nil {
		return -1, nil, fmt.Errorf("firecracker: reading the uffd handshake: %w", recvErr)
	}
	uffd := -1
	if msgs, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil && len(msgs) > 0 {
		if fds, err := unix.ParseUnixRights(&msgs[0]); err == nil && len(fds) > 0 {
			uffd = fds[0]
			for _, extra := range fds[1:] {
				unix.Close(extra)
			}
		}
	}
	if uffd < 0 {
		return -1, nil, errors.New("firecracker: the uffd handshake carried no file descriptor")
	}
	maps, err := parseUffdMappings(buf[:n])
	if err != nil {
		unix.Close(uffd)
		return -1, nil, err
	}
	return uffd, maps, nil
}

func parseUffdMappings(data []byte) ([]UffdMapping, error) {
	var maps []UffdMapping
	if err := json.Unmarshal(data, &maps); err != nil {
		return nil, fmt.Errorf("firecracker: parsing the uffd handshake: %w", err)
	}
	if len(maps) == 0 {
		return nil, errors.New("firecracker: the uffd handshake has no memory region")
	}
	ps := maps[0].PageSize
	for _, m := range maps {
		if m.PageSize != ps || ps == 0 || ps&(ps-1) != 0 || m.Size%ps != 0 || m.Offset%ps != 0 {
			return nil, fmt.Errorf("firecracker: unexpected uffd region %+v", m)
		}
	}
	return maps, nil
}

// pageServer copies pages of the memory file into the guest's memory.
type pageServer struct {
	uffd     int
	maps     []UffdMapping
	mem      []byte
	pageSize uint64
}

// prefetch copies the given file pages, contiguous runs in one call each, and returns how many it copied.
func (s *pageServer) prefetch(pages []uint32) int {
	copied := 0
	for i := 0; i < len(pages); {
		j := i + 1
		for j < len(pages) && pages[j] == pages[j-1]+1 {
			j++
		}
		off := uint64(pages[i]) * s.pageSize
		for _, m := range s.maps {
			// A run may cross regions: copy only its part inside each one.
			lo, hi := max(off, m.Offset), min(off+uint64(j-i)*s.pageSize, m.Offset+m.Size)
			if lo < hi && hi <= uint64(len(s.mem)) {
				if s.copy(m.BaseHostVirtAddr+lo-m.Offset, lo, hi-lo) == nil {
					copied += int((hi - lo) / s.pageSize)
				}
			}
		}
		i = j
	}
	return copied
}

// copy fills the guest's dst with length bytes of the file from off, waking the faulting vCPUs.
func (s *pageServer) copy(dst, off, length uint64) error {
	arg := [5]uint64{dst, uint64(uintptr(unsafe.Pointer(&s.mem[off]))), length, 0, 0}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(s.uffd), uffdioCopy, uintptr(unsafe.Pointer(&arg)))
	switch {
	case errno == 0, errno == unix.EEXIST: // EEXIST: already there, from the prefetch or a race
		return nil
	default:
		return errno
	}
}

// serve answers faults and balloon removals until stop is written. fault gets each fault's file page.
func (s *pageServer) serve(stop int, fault func(page uint32)) error {
	buf := make([]byte, uffdMsgSize*64)
	var pending []uint64 // faults to serve; one hitting EAGAIN (a removal in progress) waits for the next round
	fds := []unix.PollFd{{Fd: int32(s.uffd), Events: unix.POLLIN}, {Fd: int32(stop), Events: unix.POLLIN}}
	for {
		fds[0].Revents, fds[1].Revents = 0, 0
		timeout := -1
		if len(pending) > 0 {
			timeout = 1
		}
		if _, err := unix.Poll(fds, timeout); err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("polling the uffd: %w", err)
		}
		if fds[1].Revents != 0 {
			return nil
		}
		for {
			n, err := unix.Read(s.uffd, buf)
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) || n == 0 {
				break
			}
			if err != nil {
				return fmt.Errorf("reading the uffd: %w", err)
			}
			for msg := buf[:n]; len(msg) >= uffdMsgSize; msg = msg[uffdMsgSize:] {
				switch msg[0] {
				case uffdEventPagefault:
					pending = append(pending, binary.NativeEndian.Uint64(msg[16:]))
				case uffdEventRemove:
					// The balloon gave these pages back: unregistered, they fault in as zero pages.
					start, end := binary.NativeEndian.Uint64(msg[8:]), binary.NativeEndian.Uint64(msg[16:])
					s.unregister(start, end-start)
				}
			}
		}
		retry := pending[:0]
		for _, addr := range pending {
			page, err := s.fault(addr)
			if errors.Is(err, unix.EAGAIN) {
				retry = append(retry, addr)
				continue
			}
			if err != nil {
				return fmt.Errorf("serving a fault at %#x: %w", addr, err)
			}
			if page >= 0 {
				fault(uint32(page))
			}
		}
		pending = retry
	}
}

// fault copies the page holding addr and returns its file page, -1 when addr is in no region.
func (s *pageServer) fault(addr uint64) (int64, error) {
	page := addr &^ (s.pageSize - 1)
	for _, m := range s.maps {
		if page >= m.BaseHostVirtAddr && page < m.BaseHostVirtAddr+m.Size {
			off := m.Offset + page - m.BaseHostVirtAddr
			if off+s.pageSize > uint64(len(s.mem)) {
				return -1, fmt.Errorf("page at %#x is past the memory file", off)
			}
			return int64(off / s.pageSize), s.copy(page, off, s.pageSize)
		}
	}
	return -1, nil
}

func (s *pageServer) unregister(start, length uint64) {
	arg := [2]uint64{start, length}
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(s.uffd), uffdioUnregister, uintptr(unsafe.Pointer(&arg)))
}

// WriteWorkingSet saves pages to path as runs of little-endian uint32 (first page, count).
func WriteWorkingSet(path string, pages []uint32) error {
	var out []byte
	for i := 0; i < len(pages); {
		j := i + 1
		for j < len(pages) && pages[j] == pages[j-1]+1 {
			j++
		}
		out = binary.LittleEndian.AppendUint32(out, pages[i])
		out = binary.LittleEndian.AppendUint32(out, uint32(j-i))
		i = j
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadWorkingSet reads a file WriteWorkingSet wrote; a missing one is an empty set.
func ReadWorkingSet(path string) ([]uint32, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data)%8 != 0 {
		return nil, fmt.Errorf("firecracker: %s is not a working set", path)
	}
	var pages []uint32
	for ; len(data) > 0; data = data[8:] {
		first, n := binary.LittleEndian.Uint32(data), binary.LittleEndian.Uint32(data[4:])
		if n > 1<<22 { // 16 GiB of 4 KiB pages in one run
			return nil, fmt.Errorf("firecracker: %s has a run of %d pages", path, n)
		}
		for k := range n {
			pages = append(pages, first+k)
		}
	}
	return pages, nil
}
