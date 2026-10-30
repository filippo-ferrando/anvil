//go:build linux

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/daemon"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// stackEngine stands in for Firecracker: its clones are guestexec servers on the host.
type stackEngine struct{}

func (stackEngine) WarmSpec(p ephemeral.WarmParams) (ephemeral.WarmSpec, error) {
	return ephemeral.WarmSpec{Key: "key-" + p.Distro, Distro: p.Distro, CPUs: 2, MemoryMiB: 512}, nil
}

func (stackEngine) BuildWarm(_ context.Context, _ ephemeral.WarmSpec, dir string, _ ephemeral.Setup, progress func(string)) error {
	progress("booting")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, f := range []string{"vmstate", "mem", "disk.raw"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o640); err != nil {
			return err
		}
	}
	return nil
}

func (stackEngine) RestoreClone(_ context.Context, id string, s ephemeral.WarmSpec, _ string) (ephemeral.Clone, error) {
	if s.Distro == "broken" {
		return nil, errors.New("restore failed")
	}
	return stackClone(id), nil
}

func (stackEngine) DeleteClone(context.Context, string) error { return nil }
func (stackEngine) SweepClones(context.Context) error         { return nil }

type stackClone string

func (c stackClone) ID() string           { return string(c) }
func (c stackClone) StartedAt() time.Time { return time.Now() }
func (c stackClone) Listen(uint32) (net.Listener, error) {
	return nil, errors.New("stackClone: no vsock")
}
func (c stackClone) Dial(context.Context) (io.ReadWriteCloser, error) {
	host, guest := net.Pipe()
	go func() { _ = guestexec.Serve(guest) }()
	return host, nil
}

// captureOutput runs fn with os.Stdout and os.Stderr going to buffers.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	var bo, be bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&bo, outR); done <- struct{}{} }()
	go func() { _, _ = io.Copy(&be, errR); done <- struct{}{} }()
	fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()
	<-done
	<-done
	return bo.String(), be.String()
}

// TestMatrixThroughTheStack runs `anvil matrix` against a real EphemeralServer over a unix
// socket: request, upload, output events, summary and exit code.
func TestMatrixThroughTheStack(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := t.TempDir()
	guestexec.WorkDir = filepath.Join(root, "work")
	t.Cleanup(func() { guestexec.WorkDir = "/work" })
	mgr := ephemeral.NewManager(stackEngine{}, db, func(key string) string { return filepath.Join(root, "warm", key) })

	sock := filepath.Join(t.TempDir(), "anvild.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	anvilv1.RegisterEphemeralServiceServer(srv, daemon.NewEphemeralServer(mgr, filepath.Join(root, "uploads")))
	go func() { _ = srv.Serve(l) }()
	defer srv.Stop()

	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "hello.txt"), []byte("from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var runErr error
	stdout, stderr := captureOutput(t, func() {
		root := NewRootCommand()
		root.SetArgs([]string{"--socket", sock, "matrix", "-e", "GREETING=hi", "ubuntu,broken", project, "--", "sh", "-c", "cat hello.txt; echo $GREETING >&2"})
		runErr = root.Execute()
	})
	var exit ExitError
	if !errors.As(runErr, &exit) || exit.Code != 1 {
		t.Errorf("matrix with a broken clone = %v, want exit 1", runErr)
	}
	if stdout != "[ubuntu] from the host\n" {
		t.Errorf("stdout = %q", stdout)
	}
	for _, want := range []string{"[ubuntu] hi\n", "[broken] booting", "DISTRO", "broken  -", "restore failed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	// One distro: the command's own exit code comes back, and the warm snapshots are listed.
	stdout, _ = captureOutput(t, func() {
		root := NewRootCommand()
		root.SetArgs([]string{"--socket", sock, "launch", "--ephemeral", "ubuntu", "--", "sh", "-c", "exit 5"})
		runErr = root.Execute()
	})
	if !errors.As(runErr, &exit) || exit.Code != 5 {
		t.Errorf("launch --ephemeral exiting 5 = %v", runErr)
	}
	stdout, _ = captureOutput(t, func() {
		root := NewRootCommand()
		root.SetArgs([]string{"--socket", sock, "warm", "list"})
		runErr = root.Execute()
	})
	if runErr != nil || !strings.Contains(stdout, "key-ubuntu") || !strings.Contains(stdout, "key-broken") {
		t.Errorf("warm list = %v:\n%s", runErr, stdout)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "uploads")); len(entries) != 0 {
		t.Errorf("uploads left behind: %v", entries)
	}
}
