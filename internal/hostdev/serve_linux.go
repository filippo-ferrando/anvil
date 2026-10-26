//go:build linux

package hostdev

import (
	"encoding/json"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Serve answers one request on stdin, the connection systemd hands over with Accept=yes.
// Devices are chowned to the peer's uid, which only anvild's user can be (SocketUser, 0600).
func Serve() error {
	conn, err := net.FileConn(os.Stdin)
	if err != nil {
		return fmt.Errorf("devhelper: stdin is not a socket: %w", err)
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("devhelper: stdin is not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return fmt.Errorf("devhelper: reading the peer's credentials: %w", credErr)
	}

	var req request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return fmt.Errorf("devhelper: reading request: %w", err)
	}
	return json.NewEncoder(conn).Encode(handle(req, int(cred.Uid)))
}
