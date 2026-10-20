package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// progressStream is a server stream that keeps what was sent, for any progress message type.
type progressStream[T any] struct {
	grpc.ServerStream
	ctx  context.Context
	sent []T
}

func (s *progressStream[T]) Send(ev T) error          { s.sent = append(s.sent, ev); return nil }
func (s *progressStream[T]) Context() context.Context { return s.ctx }
func asUID(uid uint32) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: callerAddr{uid: uid}})
}

func TestInterceptorsMapNotFound(t *testing.T) {
	missing := fmt.Errorf("snapshot: %w", instance.ErrNotFound)
	_, err := interceptUnary(context.Background(), nil, nil, func(context.Context, any) (any, error) { return nil, missing })
	if status.Code(err) != codes.NotFound {
		t.Errorf("unary code = %v, want NotFound", status.Code(err))
	}
	stream := &progressStream[any]{ctx: context.Background()}
	err = interceptStream(nil, stream, nil, func(any, grpc.ServerStream) error { return missing })
	if status.Code(err) != codes.NotFound {
		t.Errorf("stream code = %v, want NotFound", status.Code(err))
	}
}

func TestExportRefusesPathCallerDoesNotOwn(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, every caller is the daemon's own user")
	}
	srv := NewExportServer(nil) // refused before the manager is reached
	stream := &progressStream[*anvilv1.ExportProgress]{ctx: asUID(54321)}
	err := srv.Export(&anvilv1.ExportRequest{Name: "web", OutputPath: filepath.Join(t.TempDir(), "b.tar.zst")}, stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("export to a dir the caller doesn't own: %v, want PermissionDenied", err)
	}
	imp := &progressStream[*anvilv1.ImportProgress]{ctx: asUID(54321)}
	err = srv.Import(&anvilv1.ImportRequest{BundlePath: "/etc/shadow"}, imp)
	if code := status.Code(err); code != codes.PermissionDenied && code != codes.InvalidArgument {
		t.Errorf("import of a file the caller doesn't own: %v, want it refused", err)
	}
}

func TestIntentApplyRefusesVolumeCallerDoesNotOwn(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, every caller is the daemon's own user")
	}
	srv := NewIntentServer(nil, nil) // refused before either manager is reached
	stream := &progressStream[*anvilv1.IntentApplyProgress]{ctx: asUID(54321)}
	manifest := "intent: app\nmembers:\n  web:\n    kind: container\n    image: nginx\n    volumes:\n      - /:/host\n"
	if err := srv.Apply(&anvilv1.IntentApplyRequest{Manifest: []byte(manifest)}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) == 0 || stream.sent[len(stream.sent)-1].GetError() == "" {
		t.Errorf("expected an in-band error for a volume the caller doesn't own, got %v", stream.sent)
	} else if e := stream.sent[len(stream.sent)-1].GetError(); !strings.Contains(e, "owned by") {
		t.Errorf("expected an ownership error, got %q", e)
	}
}
