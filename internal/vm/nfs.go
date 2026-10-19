//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// fcNFSPort is where anvild serves `anvil mount` shares to Firecracker guests, on each
// gateway address that has guests. Fixed, because the guest's fstab names it.
const fcNFSPort = 20490

// nfsHandleLimit bounds the file handles one VM's server keeps.
const nfsHandleLimit = 4096

// nfsExport is one host directory shared into one VM, served as /<tag>.
type nfsExport struct {
	hostPath string
	readOnly bool
}

// nfsHub serves every Firecracker VM's mounts. Each VM gets its own NFS server, so file
// handles from one VM's mounts never resolve in another's; one listener per gateway
// address hands each connection to the server of the VM it comes from.
type nfsHub struct {
	mu        sync.Mutex
	listeners map[netip.Addr]net.Listener // gateway -> the port guests connect to
	vms       map[netip.Addr]*nfsVM       // guest address -> its server
}

type nfsVM struct {
	exports map[string]nfsExport // tag -> share, guarded by nfsHub.mu
	conns   *connQueue
}

func newNFSHub() *nfsHub {
	// go-nfs logs client-side events as errors, like the routine fallback from an exclusive
	// create the Linux client does on every O_EXCL open.
	nfs.Log.SetLevel(nfs.FatalLevel)
	return &nfsHub{listeners: map[netip.Addr]net.Listener{}, vms: map[netip.Addr]*nfsVM{}}
}

// export shares hostPath with the guest at guest as /<tag>, listening on gateway first if needed.
func (h *nfsHub) export(gateway, guest netip.Addr, tag, hostPath string, readOnly bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.listeners[gateway]; !ok {
		l, err := net.Listen("tcp", net.JoinHostPort(gateway.String(), strconv.Itoa(fcNFSPort)))
		if err != nil {
			return fmt.Errorf("vm: serving mounts on %s: %w", gateway, err)
		}
		h.listeners[gateway] = l
		go h.accept(l)
	}
	vm, ok := h.vms[guest]
	if !ok {
		vm = &nfsVM{exports: map[string]nfsExport{}, conns: newConnQueue()}
		h.vms[guest] = vm
		handler := nfshelper.NewCachingHandler(&exportHandler{hub: h, guest: guest}, nfsHandleLimit)
		go func() { _ = nfs.Serve(vm.conns, handler) }()
	}
	vm.exports[tag] = nfsExport{hostPath: hostPath, readOnly: readOnly}
	return nil
}

// unexport stops sharing tag with guest. Files already open stay readable until closed.
func (h *nfsHub) unexport(guest netip.Addr, tag string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if vm, ok := h.vms[guest]; ok {
		delete(vm.exports, tag)
	}
}

// forget drops everything shared with guest, once its VM stopped.
func (h *nfsHub) forget(guest netip.Addr) {
	h.mu.Lock()
	vm, ok := h.vms[guest]
	delete(h.vms, guest)
	h.mu.Unlock()
	if ok {
		vm.conns.Close()
	}
}

func (h *nfsHub) lookup(guest netip.Addr, tag string) (nfsExport, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	vm, ok := h.vms[guest]
	if !ok {
		return nfsExport{}, false
	}
	e, ok := vm.exports[tag]
	return e, ok
}

func (h *nfsHub) accept(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("vm: mount server on %s: %v", l.Addr(), err)
			}
			return
		}
		addr, _ := netip.ParseAddrPort(conn.RemoteAddr().String())
		h.mu.Lock()
		vm, ok := h.vms[addr.Addr().Unmap()]
		h.mu.Unlock()
		if !ok || !vm.conns.push(conn) {
			conn.Close() // not one of our guests, or it just stopped
		}
	}
}

// exportHandler answers one VM's MOUNT requests for /<tag>.
type exportHandler struct {
	hub   *nfsHub
	guest netip.Addr
}

func (e *exportHandler) Mount(ctx context.Context, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	tag := strings.Trim(string(req.Dirpath), "/")
	export, ok := e.hub.lookup(e.guest, tag)
	if !ok {
		return nfs.MountStatusErrNoEnt, refusedFS, nil
	}
	if info, err := os.Stat(export.hostPath); err != nil || !info.IsDir() {
		return nfs.MountStatusErrNotDir, refusedFS, nil
	}
	root, err := filepath.EvalSymlinks(export.hostPath)
	if err != nil {
		return nfs.MountStatusErrNoEnt, refusedFS, nil
	}
	// BoundOS keeps every path, symlinks included, inside the shared directory.
	var fs billy.Filesystem = hostFS{osfs.New(root, osfs.WithBoundOS()), root}
	if export.readOnly {
		fs = readOnlyFS{fs}
	}
	return nfs.MountStatusOk, fs, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

func (e *exportHandler) Change(fs billy.Filesystem) billy.Change {
	c, _ := fs.(hostFS) // a readOnlyFS isn't one, so it gets no attribute changes
	return c
}

// hostFS adds attribute changes to a BoundOS share, which has none. Paths are resolved and
// kept inside root, as BoundOS does for everything else.
type hostFS struct {
	billy.Filesystem
	root string
}

func (h hostFS) hostPath(name string) (string, error) {
	p, err := filepath.EvalSymlinks(filepath.Join(h.root, filepath.Clean("/"+name)))
	if err != nil {
		return "", err
	}
	if p != h.root && !strings.HasPrefix(p, h.root+string(filepath.Separator)) {
		return "", &os.PathError{Op: "resolve", Path: name, Err: os.ErrPermission}
	}
	return p, nil
}

func (h hostFS) Chmod(name string, mode os.FileMode) error {
	p, err := h.hostPath(name)
	if err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

func (h hostFS) Chtimes(name string, atime, mtime time.Time) error {
	p, err := h.hostPath(name)
	if err != nil {
		return err
	}
	return os.Chtimes(p, atime, mtime)
}

// Chown and Lchown are accepted and ignored: files stay owned by anvild's user, like a
// root-squashed NFS export, since the guest's root can't hand host files to anyone else.
func (hostFS) Chown(string, int, int) error  { return nil }
func (hostFS) Lchown(string, int, int) error { return nil }

func (e *exportHandler) FSStat(context.Context, billy.Filesystem, *nfs.FSStat) error { return nil }

// The handle methods below are replaced by the caching handler wrapped around this one.
func (e *exportHandler) ToHandle(billy.Filesystem, []string) []byte { return nil }
func (e *exportHandler) FromHandle([]byte) (billy.Filesystem, []string, error) {
	return nil, nil, errors.New("vm: unknown NFS handle")
}
func (e *exportHandler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }
func (e *exportHandler) HandleLimit() int                                { return nfsHandleLimit }

// refusedFS goes with a refused mount: go-nfs builds a root handle even then (never sent),
// and crashes on a nil filesystem.
var refusedFS billy.Filesystem = readOnlyFS{memfs.New()}

// readOnlyFS refuses every change, so a read-only mount stays one whatever the guest does.
type readOnlyFS struct{ billy.Filesystem }

var errReadOnly = &os.PathError{Op: "write", Err: os.ErrPermission}

func (readOnlyFS) Create(string) (billy.File, error) { return nil, errReadOnly }
func (r readOnlyFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errReadOnly
	}
	return r.Filesystem.OpenFile(name, flag, perm)
}
func (readOnlyFS) Rename(string, string) error                 { return errReadOnly }
func (readOnlyFS) Remove(string) error                         { return errReadOnly }
func (readOnlyFS) TempFile(string, string) (billy.File, error) { return nil, errReadOnly }
func (readOnlyFS) MkdirAll(string, os.FileMode) error          { return errReadOnly }
func (readOnlyFS) Symlink(string, string) error                { return errReadOnly }

// connQueue is a net.Listener fed by nfsHub.accept: it hands one VM's connections to that VM's server.
type connQueue struct {
	ch     chan net.Conn
	done   chan struct{}
	closed sync.Once
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn), done: make(chan struct{})}
}

// push hands conn over, false once the queue is closed.
func (q *connQueue) push(conn net.Conn) bool {
	select {
	case q.ch <- conn:
		return true
	case <-q.done:
		return false
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case conn := <-q.ch:
		return conn, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error {
	q.closed.Do(func() { close(q.done) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{} }
