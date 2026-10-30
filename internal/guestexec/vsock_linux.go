//go:build linux

package guestexec

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// VsockListener accepts host connections on a guest vsock port.
type VsockListener struct{ fd int }

// ListenVsock listens on port for connections from any CID (in practice, the host).
func ListenVsock(port uint32) (*VsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("binding vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("listening on vsock port %d: %w", port, err)
	}
	return &VsockListener{fd: fd}, nil
}

// Accept waits for the next connection.
func (l *VsockListener) Accept() (io.ReadWriteCloser, error) {
	for {
		nfd, _, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(nfd), "vsock"), nil
	}
}

func (l *VsockListener) Close() error { return unix.Close(l.fd) }
