//go:build linux

package guestexec

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type result struct {
	code           int
	stdout, stderr string
	err            error
}

// runPipe runs r against Serve over an in-memory connection.
func runPipe(t *testing.T, ctx context.Context, r Request) result {
	t.Helper()
	host, guest := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(guest) }()
	var stdout, stderr bytes.Buffer
	r.Stdout, r.Stderr = &stdout, &stderr
	code, err := Run(ctx, host, r)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return after the command")
	}
	return result{code, stdout.String(), stderr.String(), err}
}

func cmd(argv ...string) Request { return Request{Header: Header{Argv: argv}} }

func TestServeExitCodes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		req          Request
		code         int
		stdout, errs string
	}{
		{"ok", cmd("echo", "hello"), 0, "hello\n", ""},
		{"status", cmd("sh", "-c", "echo oops >&2; exit 3"), 3, "", "oops\n"},
		{"signal", cmd("sh", "-c", "kill -TERM $$"), 128 + 15, "", ""},
		{"not found", cmd("/nonexistent/cmd"), 127, "", "anvil-guest: "},
		{"not executable", cmd("/etc/hostname"), 126, "", "anvil-guest: "},
		{"no command", cmd(), 2, "", "anvil-guest: no command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runPipe(t, ctx, tc.req)
			if got.err != nil || got.code != tc.code || got.stdout != tc.stdout || !strings.HasPrefix(got.stderr, tc.errs) ||
				(tc.errs == "" && got.stderr != "") {
				t.Errorf("got %+v, want code %d stdout %q stderr %q", got, tc.code, tc.stdout, tc.errs)
			}
		})
	}
}

func TestServeStdinEnvCwd(t *testing.T) {
	ctx := context.Background()
	if got := runPipe(t, ctx, cmd("cat")); got.code != 0 || got.stdout != "" {
		t.Errorf("cat without stdin = %+v, want an immediate end of input", got)
	}
	r := cmd("cat")
	r.Stdin = strings.NewReader("line one\nline two\n")
	if got := runPipe(t, ctx, r); got.code != 0 || got.stdout != "line one\nline two\n" {
		t.Errorf("cat = %+v", got)
	}

	dir := t.TempDir()
	r = cmd("sh", "-c", `echo "$FOO $LANG"; pwd`)
	r.Env, r.Cwd = []string{"FOO=bar", "LANG=xx"}, dir
	if got := runPipe(t, ctx, r); got.stdout != "bar xx\n"+dir+"\n" {
		t.Errorf("env and cwd = %+v", got)
	}
}

func writeTar(t *testing.T, entries ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Linkname))
			body := h.Linkname
			h.Linkname = ""
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write([]byte(body))
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// file is a regular tar entry; its body rides in Linkname until writeTar writes it.
func file(name, body string, mode int64) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Linkname: body}
}

func TestServeUpload(t *testing.T) {
	WorkDir = filepath.Join(t.TempDir(), "work")
	t.Cleanup(func() { WorkDir = "/work" })
	big := strings.Repeat("x", 3*chunkSize+17) // spans several upload frames
	upload := writeTar(t,
		&tar.Header{Typeflag: tar.TypeDir, Name: "./src/", Mode: 0o755},
		file("./src/run.sh", "#!/bin/sh\necho ran in $(pwd)\n", 0o755),
		file("big.txt", big, 0o600),
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "src/run.sh"},
		&tar.Header{Typeflag: tar.TypeFifo, Name: "fifo"},
	)
	r := cmd("sh", "-c", "./link; stat -c %a src/run.sh big.txt; wc -c < big.txt; ls")
	r.Upload = upload
	got := runPipe(t, context.Background(), r)
	want := "ran in " + WorkDir + "\n755\n600\n" + strconv.Itoa(len(big)) + "\nbig.txt\nlink\nsrc\n"
	if got.err != nil || got.code != 0 || got.stdout != want {
		t.Errorf("upload run = %+v\nwant stdout %q", got, want)
	}
}

func TestServeUploadStaysInWorkDir(t *testing.T) {
	base := t.TempDir()
	WorkDir = filepath.Join(base, "work")
	t.Cleanup(func() { WorkDir = "/work" })
	for name, upload := range map[string]*bytes.Buffer{
		"dotdot":  writeTar(t, file("../escaped", "no", 0o644)),
		"symlink": writeTar(t, &tar.Header{Typeflag: tar.TypeSymlink, Name: "out", Linkname: base}, file("out/escaped", "no", 0o644)),
	} {
		r := cmd("true")
		r.Upload = upload
		got := runPipe(t, context.Background(), r)
		if got.code != 1 || !strings.Contains(got.stderr, "upload") {
			t.Errorf("%s: got %+v, want a failed upload", name, got)
		}
		if _, err := os.Stat(filepath.Join(base, "escaped")); err == nil {
			t.Fatalf("%s: the upload wrote outside the work dir", name)
		}
	}
}

func TestServeTTY(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	r := cmd("sh", "-c", "stty size; tty; [ -t 0 ] && echo interactive; read x; stty size")
	r.TTY, r.Rows, r.Cols = true, 40, 100
	resize := make(chan Size, 1)
	stdinR, stdinW := io.Pipe()
	r.Resize, r.Stdin = resize, stdinR
	go func() {
		time.Sleep(300 * time.Millisecond)
		resize <- Size{Rows: 50, Cols: 120}
		time.Sleep(200 * time.Millisecond)
		_, _ = stdinW.Write([]byte("go\n"))
	}()
	got := runPipe(t, context.Background(), r)
	for _, want := range []string{"40 100", "/dev/pts/", "interactive", "50 120"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("tty output lacks %q: %+v", want, got)
		}
	}
	if got.code != 0 || got.stderr != "" {
		t.Errorf("tty run = %+v", got)
	}
}

func TestServeDisconnectKills(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(pidFile); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	got := runPipe(t, ctx, cmd("sh", "-c", "echo $$ > "+pidFile+"; exec sleep 30"))
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Run = %+v, want a cancel", got)
	}
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the command (pid %d) survived the disconnect", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestServeBackgroundChildDoesNotHang(t *testing.T) {
	start := time.Now()
	got := runPipe(t, context.Background(), cmd("sh", "-c", "sleep 30 & echo done"))
	if got.code != 0 || got.stdout != "done\n" {
		t.Errorf("got %+v", got)
	}
	if took := time.Since(start); took > drainTimeout {
		t.Errorf("took %s: the leftover child held the output open", took)
	}
}

func TestServeRejectsOtherVersion(t *testing.T) {
	host, guest := net.Pipe()
	go func() { _ = Serve(guest) }()
	fw := &frameWriter{w: host}
	data, _ := json.Marshal(Header{Version: Version + 1, Argv: []string{"true"}})
	if err := fw.write(frameHeader, data); err != nil {
		t.Fatal(err)
	}
	var stderr string
	for {
		typ, payload, err := readFrame(host)
		if err != nil {
			t.Fatal(err)
		}
		if typ == frameStderr {
			stderr += string(payload)
		}
		if typ == frameExit {
			if code := int32(payload[3]); code != 2 || !strings.Contains(stderr, "protocol version") {
				t.Errorf("exit %d, stderr %q", code, stderr)
			}
			return
		}
	}
}

// TestServeSetClock can't set the clock without CAP_SYS_TIME, so it checks the request
// arrives and its failure comes back as an error.
func TestServeSetClock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root this would really set the clock")
	}
	host, guest := net.Pipe()
	go func() { _ = Serve(guest) }()
	err := SetupClone(context.Background(), host, time.Now(), "")
	if err == nil || !strings.Contains(err.Error(), "setting the clock") {
		t.Errorf("SetupClone without CAP_SYS_TIME = %v", err)
	}
}

func TestRenameInHosts(t *testing.T) {
	for _, tc := range []struct{ hosts, want string }{
		{"127.0.0.1 localhost\n127.0.1.1 eph-wabc.localdomain eph-wabc # cloud-init\n",
			"127.0.0.1 localhost\n127.0.1.1 eph-new.localdomain eph-new # cloud-init\n"},
		{"127.0.0.1 localhost\n", "127.0.0.1 localhost\n127.0.1.1 eph-new\n"},
		{"127.0.0.1 localhost", "127.0.0.1 localhost\n127.0.1.1 eph-new\n"},
		{"", "127.0.1.1 eph-new\n"},
		{"10.0.0.1 eph-wabcd\n", "10.0.0.1 eph-wabcd\n127.0.1.1 eph-new\n"},
	} {
		if got := renameInHosts(tc.hosts, "eph-wabc", "eph-new"); got != tc.want {
			t.Errorf("renameInHosts(%q) = %q, want %q", tc.hosts, got, tc.want)
		}
	}
}

// TestServeHostname checks a bad name is refused before anything changes.
func TestServeHostname(t *testing.T) {
	host, guest := net.Pipe()
	go func() { _ = Serve(guest) }()
	var stderr strings.Builder
	code, err := Run(context.Background(), host, Request{Header: Header{Hostname: "bad name"}, Stderr: &stderr})
	if err != nil || code != 1 || !strings.Contains(stderr.String(), "invalid hostname") {
		t.Errorf("bad hostname = %d, %v, %q", code, err, stderr.String())
	}
}

// TestServeSetsHostname renames the host it runs on, so it needs its own user and UTS
// namespaces: run under `unshare -r --uts`.
func TestServeSetsHostname(t *testing.T) {
	data, _ := os.ReadFile("/proc/self/uid_map")
	if os.Geteuid() != 0 || strings.Join(strings.Fields(string(data)), " ") == "0 0 4294967295" {
		t.Skip("needs its own user and UTS namespaces, run under unshare -r --uts")
	}
	old, _ := os.Hostname()
	hostsFile = filepath.Join(t.TempDir(), "hosts")
	t.Cleanup(func() { hostsFile = "/etc/hosts" })
	if err := os.WriteFile(hostsFile, []byte("127.0.1.1 "+old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	go func() { _ = Serve(guest) }()
	var stderr strings.Builder
	if code, err := Run(context.Background(), host, Request{Header: Header{Hostname: "eph-test1"}, Stderr: &stderr}); err != nil || code != 0 {
		t.Fatalf("set hostname = %d, %v, %q", code, err, stderr.String())
	}
	got, _ := os.Hostname()
	hosts, _ := os.ReadFile(hostsFile)
	if got != "eph-test1" || string(hosts) != "127.0.1.1 eph-test1\n" {
		t.Errorf("hostname %q, hosts %q", got, hosts)
	}
}
