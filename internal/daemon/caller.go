package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"git.fferrando.cc/rdfilippo/anvil/internal/hostpath"
)

// CallerListener wraps the daemon's unix socket listener so every connection carries
// its peer's uid, which handlers read back through peer.FromContext.
func CallerListener(l net.Listener) net.Listener { return callerListener{l} }

type callerListener struct{ net.Listener }

func (l callerListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return c, nil
	}
	uid, err := peerUID(uc)
	if err != nil {
		log.Printf("daemon: reading the peer's credentials: %v", err)
		return c, nil // callerUID then finds nothing, and checks fail closed
	}
	return callerConn{Conn: c, uid: uid}, nil
}

type callerConn struct {
	net.Conn
	uid uint32
}

func (c callerConn) RemoteAddr() net.Addr { return callerAddr{uid: c.uid} }

type callerAddr struct{ uid uint32 }

func (callerAddr) Network() string  { return "unix" }
func (a callerAddr) String() string { return fmt.Sprintf("uid %d", a.uid) }

func callerUID(ctx context.Context) (uint32, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return 0, false
	}
	a, ok := p.Addr.(callerAddr)
	return a.uid, ok
}

// checkCallerOwns resolves each host path in place and fails unless the caller owns it.
// The daemon reaches more than the caller (Docker binds as root), so a group member could
// otherwise share "/" into an instance. Root and the daemon's own user may share anything.
func checkCallerOwns(ctx context.Context, paths ...*string) error {
	if len(paths) == 0 {
		return nil
	}
	uid, ok := callerUID(ctx)
	if !ok {
		return status.Error(codes.PermissionDenied, "daemon: can't identify the caller to check its host paths")
	}
	for _, p := range paths {
		real, owner, err := hostpath.Resolve(*p)
		if errors.Is(err, fs.ErrPermission) {
			return status.Errorf(codes.PermissionDenied, "daemon: host path %s: %v%s", *p, err, hostpath.Hint(*p))
		} else if err != nil {
			return status.Errorf(codes.InvalidArgument, "daemon: host path %s: %v", *p, err)
		}
		if uid != 0 && int(uid) != os.Getuid() && owner != uid {
			return status.Errorf(codes.PermissionDenied, "daemon: %s is owned by uid %d, not by the caller (uid %d); only a directory the caller owns can be shared", *p, owner, uid)
		}
		*p = real
	}
	return nil
}
