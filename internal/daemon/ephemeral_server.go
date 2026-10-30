package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// EphemeralServer implements anvilv1.EphemeralServiceServer against an ephemeral.Manager.
type EphemeralServer struct {
	anvilv1.UnimplementedEphemeralServiceServer
	Manager *ephemeral.Manager
	// UploadDir holds a run's upload while its clones read it.
	UploadDir string
}

func NewEphemeralServer(m *ephemeral.Manager, uploadDir string) *EphemeralServer {
	return &EphemeralServer{Manager: m, UploadDir: uploadDir}
}

func ephemeralErr(err error) error {
	if errors.Is(err, ephemeral.ErrUnsupported) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return wrapErr(err)
}

func warmToPB(w store.WarmSnapshot) *anvilv1.WarmSnapshot {
	return &anvilv1.WarmSnapshot{
		Key: w.Key, Distro: w.Distro, Arch: w.Arch, Cpus: int32(w.CPUs), MemoryMib: w.MemoryMiB, DiskGib: w.DiskGiB,
		FirecrackerVersion: w.FirecrackerVersion, SizeBytes: w.SizeBytes,
		CreatedAtUnix: w.CreatedAt.Unix(), LastUsedAtUnix: w.LastUsedAt.Unix(), App: w.App, X11: w.X11,
	}
}

func sizeFromPB(s *anvilv1.TerminalSize) guestexec.Size {
	return guestexec.Size{Rows: uint16(s.GetRows()), Cols: uint16(s.GetCols())}
}

// stdinFeed turns the stdin, close and resize inputs that follow a request into a reader
// and a channel for guestexec, and stops once recv fails.
type stdinFeed struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	resize chan guestexec.Size
}

func newStdinFeed() *stdinFeed {
	r, w := io.Pipe()
	return &stdinFeed{r: r, w: w, resize: make(chan guestexec.Size, 4)}
}

func (f *stdinFeed) handle(stdin []byte, closeIn bool, resize *anvilv1.TerminalSize) {
	switch {
	case stdin != nil:
		_, _ = f.w.Write(stdin)
	case closeIn:
		f.w.Close()
	case resize != nil:
		select {
		case f.resize <- sizeFromPB(resize):
		default: // a newer size follows
		}
	}
}

func (s *EphemeralServer) Run(stream anvilv1.EphemeralService_RunServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	req := first.GetRequest()
	if req == nil {
		return status.Error(codes.InvalidArgument, "ephemeral: a run starts with its request")
	}
	r := ephemeral.RunRequest{
		Distros: req.GetDistros(), Arch: req.GetArch(), Argv: req.GetCommand(), Env: req.GetEnv(), Cwd: req.GetCwd(),
		CPUs: int(req.GetCpus()), MemoryMiB: req.GetMemoryMib(), KeepFailed: req.GetKeepFailed(),
		TTY: req.GetTty(), Size: sizeFromPB(req.GetSize()),
	}
	if len(r.Distros) == 0 || len(r.Argv) == 0 {
		return status.Error(codes.InvalidArgument, "ephemeral: a run needs at least one distro and a command")
	}
	if req.GetUpload() {
		path, err := s.receiveUpload(stream)
		if err != nil {
			return err
		}
		defer os.Remove(path)
		r.Upload = path
	}
	// Only a run on one distro takes input; it ends with the stream.
	feed := newStdinFeed()
	defer feed.w.Close()
	if len(r.Distros) == 1 {
		r.Stdin, r.Resize = feed.r, feed.resize
	}
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				feed.w.Close()
				return
			}
			feed.handle(in.GetStdin(), in.GetStdinClose(), in.GetResize())
		}
	}()

	var sendErr error
	err = s.Manager.Run(ctx, r, func(ev ephemeral.Event) {
		if sendErr == nil {
			sendErr = stream.Send(runEventToPB(ev))
		}
	})
	if err != nil {
		return ephemeralErr(err)
	}
	return sendErr
}

// receiveUpload spools the upload chunks into a file every clone reads on its own.
func (s *EphemeralServer) receiveUpload(stream anvilv1.EphemeralService_RunServer) (string, error) {
	if err := os.MkdirAll(s.UploadDir, 0o750); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.UploadDir, "upload-*.tar")
	if err != nil {
		return "", err
	}
	defer f.Close()
	for {
		in, err := stream.Recv()
		if err == nil && in.GetUploadDone() {
			return f.Name(), nil
		}
		if err == nil && in.GetUploadChunk() == nil {
			err = status.Error(codes.InvalidArgument, "ephemeral: the upload was interrupted")
		}
		if err == nil {
			_, err = f.Write(in.GetUploadChunk())
		}
		if err != nil {
			os.Remove(f.Name())
			return "", err
		}
	}
}

func runEventToPB(ev ephemeral.Event) *anvilv1.RunEvent {
	switch {
	case ev.Progress != nil:
		p := ev.Progress
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Progress{Progress: &anvilv1.RunProgress{
			CloneId: p.CloneID, Distro: p.Distro, Message: p.Message}}}
	case ev.Output != nil:
		o := ev.Output
		st := anvilv1.OutputStream_OUTPUT_STREAM_STDOUT
		if o.Stderr {
			st = anvilv1.OutputStream_OUTPUT_STREAM_STDERR
		}
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{
			CloneId: o.CloneID, Distro: o.Distro, Stream: st, Data: o.Data}}}
	default:
		x := ev.Exit
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Exit{Exit: &anvilv1.RunExit{
			CloneId: x.CloneID, Distro: x.Distro, ExitCode: int32(x.Code), DurationMs: x.Duration.Milliseconds(),
			Kept: x.Kept, Error: x.Err}}}
	}
}

func (s *EphemeralServer) WarmCreate(req *anvilv1.WarmCreateRequest, stream anvilv1.EphemeralService_WarmCreateServer) error {
	if req.GetDistro() == "" {
		return status.Error(codes.InvalidArgument, "ephemeral: a warm snapshot needs a distro")
	}
	p := ephemeral.WarmParams{Distro: req.GetDistro(), Arch: req.GetArch(), CPUs: int(req.GetCpus()), MemoryMiB: req.GetMemoryMib()}
	rec, _, err := s.Manager.Warm(stream.Context(), p, req.GetRebuild(), func(msg string) {
		_ = stream.Send(&anvilv1.WarmCreateProgress{Status: msg})
	})
	if err != nil {
		return ephemeralErr(err)
	}
	return stream.Send(&anvilv1.WarmCreateProgress{Status: "ready", Snapshot: warmToPB(rec)})
}

func (s *EphemeralServer) WarmList(context.Context, *anvilv1.WarmListRequest) (*anvilv1.WarmListReply, error) {
	list, err := s.Manager.ListWarm()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.WarmListReply{}
	for _, w := range list {
		reply.Snapshots = append(reply.Snapshots, warmToPB(w))
	}
	return reply, nil
}

func (s *EphemeralServer) WarmDelete(_ context.Context, req *anvilv1.WarmDeleteRequest) (*anvilv1.WarmDeleteReply, error) {
	if len(req.GetKeys()) == 0 && !req.GetAll() {
		return nil, status.Error(codes.InvalidArgument, "ephemeral: name the warm snapshots to delete, or all")
	}
	deleted, err := s.Manager.DeleteWarm(req.GetKeys(), req.GetAll())
	return &anvilv1.WarmDeleteReply{Deleted: deleted}, ephemeralErr(err)
}

func (s *EphemeralServer) CloneList(context.Context, *anvilv1.CloneListRequest) (*anvilv1.CloneListReply, error) {
	reply := &anvilv1.CloneListReply{}
	for _, c := range s.Manager.Clones() {
		reply.Clones = append(reply.Clones, &anvilv1.EphemeralClone{
			Id: c.ID, Distro: c.Distro, WarmKey: c.WarmKey, StartedAtUnix: c.StartedAt.Unix(),
			Exited: c.Exited, ExitCode: int32(c.ExitCode), Kept: c.Kept,
		})
	}
	return reply, nil
}

func (s *EphemeralServer) CloneExec(stream anvilv1.EphemeralService_CloneExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	req := first.GetRequest()
	if req == nil || req.GetCloneId() == "" {
		return status.Error(codes.InvalidArgument, "ephemeral: an exec starts with its request, naming the clone")
	}
	argv := req.GetCommand()
	if len(argv) == 0 {
		argv = []string{"/bin/sh", "-c", `exec "$(command -v bash || echo sh)" -l`}
	}
	feed := newStdinFeed()
	defer feed.w.Close()
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				feed.w.Close()
				return
			}
			feed.handle(in.GetStdin(), in.GetStdinClose(), in.GetResize())
		}
	}()
	var mu sync.Mutex // grpc streams take one Send at a time
	send := func(out *anvilv1.CloneExecOutput) {
		mu.Lock()
		defer mu.Unlock()
		_ = stream.Send(out)
	}
	code, err := s.Manager.Exec(stream.Context(), req.GetCloneId(), guestexec.Request{
		Header: guestexec.Header{Argv: argv, Env: req.GetEnv(), TTY: req.GetTty(), Rows: uint16(req.GetSize().GetRows()), Cols: uint16(req.GetSize().GetCols())},
		Stdin:  feed.r, Resize: feed.resize,
		Stdout: sendWriter(func(p []byte) { send(&anvilv1.CloneExecOutput{Output: &anvilv1.CloneExecOutput_Stdout{Stdout: p}}) }),
		Stderr: sendWriter(func(p []byte) { send(&anvilv1.CloneExecOutput{Output: &anvilv1.CloneExecOutput_Stderr{Stderr: p}}) }),
	})
	if err != nil {
		return ephemeralErr(fmt.Errorf("ephemeral: %w", err))
	}
	return stream.Send(&anvilv1.CloneExecOutput{Output: &anvilv1.CloneExecOutput_ExitCode{ExitCode: int32(code)}})
}

type sendWriter func(p []byte)

func (w sendWriter) Write(p []byte) (int, error) {
	w(append([]byte(nil), p...))
	return len(p), nil
}

func (s *EphemeralServer) CloneDelete(ctx context.Context, req *anvilv1.CloneDeleteRequest) (*anvilv1.CloneDeleteReply, error) {
	if len(req.GetIds()) == 0 && !req.GetAll() {
		return nil, status.Error(codes.InvalidArgument, "ephemeral: name the clones to delete, or all")
	}
	deleted, err := s.Manager.DeleteClones(ctx, req.GetIds(), req.GetAll())
	return &anvilv1.CloneDeleteReply{Deleted: deleted}, ephemeralErr(err)
}

// appDisplayDir is where a RunApp client's waypipe listens; a var for tests.
var appDisplayDir = func(id string) string { return config.GUIRelayDir(id) }

func (s *EphemeralServer) RunApp(stream anvilv1.EphemeralService_RunAppServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	req := first.GetRequest()
	if req == nil {
		return status.Error(codes.InvalidArgument, "ephemeral: an app run starts with its request")
	}
	if req.GetDistro() == "" || req.GetPackage() == "" {
		return status.Error(codes.InvalidArgument, "ephemeral: an app run needs a distro and a package")
	}
	if err := ephemeral.ValidPackage(req.GetPackage()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	r := ephemeral.AppRequest{
		Distro: req.GetDistro(), Arch: req.GetArch(), Package: req.GetPackage(), Command: req.GetCommand(),
		Env: req.GetEnv(), X11: req.GetX11(), CPUs: int(req.GetCpus()), MemoryMiB: req.GetMemoryMib(), InfoOnly: req.GetInfoOnly(),
	}
	var mu sync.Mutex // grpc streams take one Send at a time
	var sendErr error
	send := func(ev *anvilv1.AppEvent) {
		mu.Lock()
		defer mu.Unlock()
		if sendErr == nil {
			sendErr = stream.Send(ev)
		}
	}
	if !r.InfoOnly {
		// World-writable like a container's relay dir: the client's user varies, and the
		// parent run dir keeps other host users out.
		dir := appDisplayDir("app-" + ulid.Make().String())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return wrapErr(err)
		}
		defer os.RemoveAll(dir)
		if err := os.Chmod(dir, 0o777); err != nil {
			return wrapErr(err)
		}
		ready := make(chan struct{})
		r.Display, r.DisplayReady = filepath.Join(dir, gui.AppDisplaySocket), ready
		go func() {
			once := false
			for {
				in, err := stream.Recv()
				if err != nil {
					return
				}
				if in.GetDisplayReady() && !once {
					once = true
					close(ready)
				}
			}
		}()
		send(&anvilv1.AppEvent{Event: &anvilv1.AppEvent_Session{Session: &anvilv1.AppSession{DisplayDir: dir}}})
	}
	err = s.Manager.RunApp(ctx, r, func(ev ephemeral.Event) {
		if ev.App == nil {
			re := runEventToPB(ev)
			switch e := re.GetEvent().(type) {
			case *anvilv1.RunEvent_Progress:
				send(&anvilv1.AppEvent{Event: &anvilv1.AppEvent_Progress{Progress: e.Progress}})
			case *anvilv1.RunEvent_Output:
				send(&anvilv1.AppEvent{Event: &anvilv1.AppEvent_Output{Output: e.Output}})
			case *anvilv1.RunEvent_Exit:
				send(&anvilv1.AppEvent{Event: &anvilv1.AppEvent_Exit{Exit: e.Exit}})
			}
			return
		}
		a := ev.App
		send(&anvilv1.AppEvent{Event: &anvilv1.AppEvent_App{App: &anvilv1.AppStarted{
			CloneId: a.CloneID, Distro: a.Distro, Command: a.Command,
			Info: &anvilv1.GUIAppInfoReply{Found: a.FromEntry, AppName: a.Name, Icon: a.Icon, IconExt: a.IconExt, IconSize: int32(a.IconSize)},
		}}})
	})
	if err != nil {
		return ephemeralErr(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return sendErr
}
