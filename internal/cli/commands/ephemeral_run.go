package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// ephemeralFlags are the flags `launch --ephemeral` and `matrix` share.
type ephemeralFlags struct {
	cpus       int32
	memoryMiB  int64
	arch       string
	env        []string
	keepFailed bool
	logs       string
	tty        bool
	collect    []string
	perDistro  bool // collect into a directory per distro, as matrix does
	setup      string
	keyFiles   []string
}

func (f *ephemeralFlags) addRunFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.keepFailed, "keep-failed", false, "keep a clone whose command failed, for `anvil ephemeral shell`")
	cmd.Flags().StringVar(&f.logs, "logs", "", "write each distro's full output to <dir>/<distro>.log")
	cmd.Flags().StringArrayVar(&f.collect, "collect", nil, "copy <guest-path>[:<host-dir>] back after the command, failed or not; "+
		"relative to /work with an upload, globs and ** allowed, host dir default . (repeatable)")
}

// splitRunArgs splits `<distros> [path] -- <command>`: a path can't follow `--`, where it
// would read as the command's own argument.
func splitRunArgs(cmd *cobra.Command, args []string) (first, path string, command []string, err error) {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		return "", "", nil, errors.New("put the command after `--`, e.g. -- make test")
	}
	before, command := args[:dash], args[dash:]
	if len(before) == 0 || len(before) > 2 || len(command) == 0 {
		return "", "", nil, errors.New("want <distro> [path] -- <command>")
	}
	if len(before) == 2 {
		path = before[1]
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return "", "", nil, fmt.Errorf("%s is not a directory to upload", path)
		}
	}
	return before[0], path, command, nil
}

// runEphemeral runs command on one clone per distro and prints their output, one line
// at a time with a [distro] prefix (or straight through on a terminal), then a summary.
func runEphemeral(cmd *cobra.Command, flags *globalFlags, distros []string, path string, command []string, f ephemeralFlags) error {
	if f.tty && len(distros) > 1 {
		return errors.New("--tty is for a run on one distro")
	}
	setup, err := client.LoadSetup(f.setup, f.keyFiles)
	if err != nil {
		return err
	}
	specs := make([]client.CollectSpec, len(f.collect))
	guestPaths := make([]string, len(f.collect))
	for i, s := range f.collect {
		spec, err := client.ParseCollect(s)
		if err != nil {
			return err
		}
		specs[i], guestPaths[i] = spec, spec.Guest
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := dial(flags)
	if err != nil {
		return err
	}
	defer c.Close()

	// Cancelled when the upload fails, so the daemon stops waiting for the rest of it.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stream, err := c.Ephemeral.Run(ctx)
	if err != nil {
		return err
	}
	var sendMu sync.Mutex
	send := func(in *anvilv1.RunInput) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(in)
	}
	req := &anvilv1.RunRequest{
		Distros: distros, Command: command, Env: f.env, Cpus: f.cpus, MemoryMib: f.memoryMiB, Arch: f.arch,
		KeepFailed: f.keepFailed, Upload: path != "", Tty: f.tty, Collect: guestPaths, Setup: setup,
	}
	if f.tty {
		if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
			req.Size = &anvilv1.TerminalSize{Rows: uint32(h), Cols: uint32(w)}
		}
	}
	if err := send(&anvilv1.RunInput{Input: &anvilv1.RunInput_Request{Request: req}}); err != nil {
		return err
	}

	out := newRunPrinter(os.Stdout, os.Stderr, distros, f.tty)
	if len(specs) > 0 {
		out.collector = client.NewCollector(guestPaths, func(i int, distro string) string {
			return client.CollectDir(specs[i], distro, f.perDistro)
		})
		defer out.collector.Close()
	}
	stopLive := out.live()
	defer stopLive()
	if f.logs != "" {
		if err := out.openLogs(f.logs); err != nil {
			return err
		}
		defer out.closeLogs()
	}
	// The daemon restores the clones while the upload goes, so it runs alongside the events.
	// A terminal run sends it first: raw keystrokes must not start before it ends.
	restore := func() {}
	uploadErr := make(chan error, 1)
	if f.tty {
		if path != "" {
			if err := sendUpload(path, send); err != nil {
				return err
			}
		}
		restore = forwardInput(f.tty, len(distros) == 1, send)
	} else {
		go func() {
			if path != "" {
				if err := sendUpload(path, send); err != nil {
					uploadErr <- err
					cancelRun()
					return
				}
			}
			forwardInput(false, len(distros) == 1, send) // no terminal to restore
		}()
	}
	defer func() { restore() }()
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			restore()
			out.stopBoard()
			select {
			case upErr := <-uploadErr: // the stream broke because the upload did
				return upErr
			default:
			}
			if status.Code(err) == codes.Canceled || ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "anvil: cancelled, the clones are deleted")
				return ExitError{Code: 130}
			}
			return err
		}
		out.event(ev)
	}
	restore()
	out.finish()
	return ExitError{Code: out.exitCode()}.orNil()
}

// ExitError ends anvil with Code and no message of its own, for a remote command's exit code.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

func (e ExitError) orNil() error {
	if e.Code == 0 {
		return nil
	}
	return e
}

// sendUpload streams path as a tar in upload chunks.
func sendUpload(path string, send func(*anvilv1.RunInput) error) error {
	err := client.SendUpload(path, func(chunk []byte) error {
		return send(&anvilv1.RunInput{Input: &anvilv1.RunInput_UploadChunk{UploadChunk: chunk}})
	})
	if err != nil {
		return err
	}
	return send(&anvilv1.RunInput{Input: &anvilv1.RunInput_UploadDone{UploadDone: true}})
}

// forwardInput sends stdin to a one-distro run: raw keystrokes and resizes with tty, a piped
// stdin as it is otherwise. It returns the func that puts the terminal back.
func forwardInput(tty, single bool, send func(*anvilv1.RunInput) error) func() {
	noop := func() {}
	if !single || (!tty && term.IsTerminal(os.Stdin.Fd())) {
		return noop // nothing to read: the command gets an empty stdin
	}
	restore := noop
	if tty && term.IsTerminal(os.Stdin.Fd()) {
		if state, err := term.MakeRaw(os.Stdin.Fd()); err == nil {
			var once sync.Once
			restore = func() { once.Do(func() { _ = term.Restore(os.Stdin.Fd(), state) }) }
		}
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
					_ = send(&anvilv1.RunInput{Input: &anvilv1.RunInput_Resize{Resize: &anvilv1.TerminalSize{Rows: uint32(h), Cols: uint32(w)}}})
				}
			}
		}()
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if send(&anvilv1.RunInput{Input: &anvilv1.RunInput_Stdin{Stdin: append([]byte(nil), buf[:n]...)}}) != nil {
					return
				}
			}
			if err != nil {
				_ = send(&anvilv1.RunInput{Input: &anvilv1.RunInput_StdinClose{StdinClose: true}})
				return
			}
		}
	}()
	return restore
}

// runPrinter turns a run's events into prefixed lines, logs and a summary.
type runPrinter struct {
	stdout, stderr io.Writer
	raw            bool // a terminal run: output goes through untouched
	color          bool
	width          int
	colors         map[string]string
	prefixes       map[string]string        // distro -> its line prefix, made once
	partial        map[string]*bytes.Buffer // distro + stream -> a line not ended yet
	logs           map[string]*os.File
	exits          map[string]*anvilv1.RunExit
	order          []string
	board          *statusBoard      // live steps at the bottom of a terminal, nil elsewhere
	collector      *client.Collector // nil without --collect
	label          string            // the summary's first column, DISTRO by default
}

var prefixColors = []string{"36", "33", "35", "32", "34", "91", "96", "93"}

func newRunPrinter(stdout, stderr io.Writer, distros []string, raw bool) *runPrinter {
	p := &runPrinter{
		stdout: stdout, stderr: stderr, raw: raw, color: isTerminalWriter(stdout) && os.Getenv("NO_COLOR") == "",
		colors: map[string]string{}, partial: map[string]*bytes.Buffer{}, exits: map[string]*anvilv1.RunExit{}, order: distros,
	}
	for i, d := range distros {
		p.colors[d] = prefixColors[i%len(prefixColors)]
		p.width = max(p.width, len(d))
	}
	// A -t run's terminal is in raw mode, where the board's line moves don't hold.
	if !raw && isTerminalWriter(stderr) && os.Getenv("TERM") != "dumb" {
		p.board = newStatusBoard(stderr, distros)
	}
	return p
}

// live starts the board's spinner, when there is a board; the returned func stops it.
func (p *runPrinter) live() func() {
	if p.board == nil {
		return func() {}
	}
	return p.board.start()
}

// stopBoard takes the board off screen for good.
func (p *runPrinter) stopBoard() {
	if p.board != nil {
		p.board.stop()
	}
}

// print runs write above the board, or straight away without one.
func (p *runPrinter) print(write func()) {
	if p.board == nil {
		write()
		return
	}
	p.board.print(write)
}

func (p *runPrinter) openLogs(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p.logs = map[string]*os.File{}
	for _, d := range p.order {
		f, err := os.Create(filepath.Join(dir, strings.ReplaceAll(d, "/", "_")+".log"))
		if err != nil {
			return err
		}
		p.logs[d] = f
	}
	return nil
}

func (p *runPrinter) closeLogs() {
	for _, f := range p.logs {
		f.Close()
	}
}

func (p *runPrinter) prefix(distro string) string {
	if s, ok := p.prefixes[distro]; ok {
		return s
	}
	label := fmt.Sprintf("[%-*s]", p.width, distro)
	s := label + " "
	if p.color {
		s = "\x1b[" + p.colors[distro] + "m" + label + "\x1b[0m "
	}
	if p.prefixes == nil {
		p.prefixes = map[string]string{}
	}
	p.prefixes[distro] = s
	return s
}

func (p *runPrinter) event(ev *anvilv1.RunEvent) {
	switch {
	case ev.GetProgress() != nil:
		pr := ev.GetProgress()
		if p.board != nil {
			p.board.set(pr.GetDistro(), pr.GetMessage(), func(d, line string) {
				fmt.Fprintf(p.stderr, "%s%s\n", p.prefix(d), dim(p.color, line))
			})
			return
		}
		eol := "\n"
		if p.raw {
			eol = "\r\n" // the terminal is in raw mode
		}
		fmt.Fprintf(p.stderr, "%s%s%s", p.prefix(pr.GetDistro()), dim(p.color, pr.GetMessage()), eol)
	case ev.GetOutput() != nil:
		o := ev.GetOutput()
		if f := p.logs[o.GetDistro()]; f != nil {
			_, _ = f.Write(o.GetData())
		}
		w := p.stdout
		if o.GetStream() == anvilv1.OutputStream_OUTPUT_STREAM_STDERR {
			w = p.stderr
		}
		if p.raw {
			_, _ = w.Write(o.GetData())
			return
		}
		p.print(func() { p.writeLines(o.GetDistro(), o.GetStream(), w, o.GetData()) })
	case ev.GetCollect() != nil:
		if p.collector == nil {
			return
		}
		if res := p.collector.Handle(ev.GetCollect()); res != nil {
			p.collected(res)
		}
	case ev.GetExit() != nil:
		x := ev.GetExit()
		p.print(func() { p.flush(x.GetDistro()) })
		if p.board != nil {
			p.board.done(x.GetDistro())
		}
		p.exits[x.GetDistro()] = x
	}
}

// collected prints where a collect path of one clone went, or why it didn't.
func (p *runPrinter) collected(res *client.CollectResult) {
	msg := fmt.Sprintf("collected %s into %s", res.Path, res.Dir)
	if res.Err != nil {
		msg = "collect: " + res.Err.Error()
	}
	eol := "\n"
	if p.raw {
		eol = "\r\n"
	}
	p.print(func() {
		p.flush(res.Distro) // the command's last line, which may lack its newline
		fmt.Fprintf(p.stderr, "%s%s%s", p.prefix(res.Distro), dim(p.color, msg), eol)
	})
}

// writeLines prints every complete line of data with the distro's prefix, and keeps the
// rest until its newline arrives, so lines of different clones never mix.
func (p *runPrinter) writeLines(distro string, st anvilv1.OutputStream, w io.Writer, data []byte) {
	key := distro + "\x00" + st.String()
	buf := p.partial[key]
	if buf == nil {
		buf = &bytes.Buffer{}
		p.partial[key] = buf
	}
	buf.Write(data)
	// Every complete line goes out in one write, not one per line.
	var out bytes.Buffer
	prefix := p.prefix(distro)
	for {
		line, err := buf.ReadBytes('\n')
		if err != nil {
			buf.Write(line) // not a full line yet
			break
		}
		out.WriteString(prefix)
		out.Write(line)
	}
	if out.Len() > 0 {
		_, _ = w.Write(out.Bytes())
	}
}

// flush prints distro's unfinished lines, once its command is done.
func (p *runPrinter) flush(distro string) {
	for _, st := range []anvilv1.OutputStream{anvilv1.OutputStream_OUTPUT_STREAM_STDOUT, anvilv1.OutputStream_OUTPUT_STREAM_STDERR} {
		if buf := p.partial[distro+"\x00"+st.String()]; buf != nil && buf.Len() > 0 {
			w := p.stdout
			if st == anvilv1.OutputStream_OUTPUT_STREAM_STDERR {
				w = p.stderr
			}
			fmt.Fprintf(w, "%s%s\n", p.prefix(distro), buf.Bytes())
			buf.Reset()
		}
	}
}

// finish prints the summary table: one row per distro, in the order asked.
func (p *runPrinter) finish() {
	p.stopBoard()
	for _, d := range p.order {
		p.flush(d)
	}
	if p.raw && len(p.order) == 1 {
		if x := p.exits[p.order[0]]; x != nil && x.GetError() != "" {
			fmt.Fprintf(p.stderr, "anvil: %s: %s\n", x.GetDistro(), x.GetError())
		}
		return
	}
	fmt.Fprintln(p.stderr)
	tw := tabwriter.NewWriter(p.stderr, 0, 4, 2, ' ', 0)
	label := p.label
	if label == "" {
		label = "DISTRO"
	}
	fmt.Fprintln(tw, label+"\tEXIT\tDURATION\tNOTE")
	for _, d := range p.order {
		x := p.exits[d]
		if x == nil {
			fmt.Fprintf(tw, "%s\t-\t-\tno result\n", d)
			continue
		}
		note := ""
		switch {
		case x.GetError() != "":
			note = x.GetError()
		case x.GetKept():
			note = "kept as " + x.GetCloneId()
		}
		exit := fmt.Sprint(x.GetExitCode())
		if x.GetExitCode() < 0 {
			exit = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d, exit, (time.Duration(x.GetDurationMs()) * time.Millisecond).Round(10*time.Millisecond), note)
	}
	tw.Flush()
}

// exitCode is the command's own exit code for one distro, 1 for a matrix with any failure.
func (p *runPrinter) exitCode() int {
	failed := false
	for _, d := range p.order {
		x := p.exits[d]
		if x == nil || x.GetExitCode() != 0 {
			failed = true
		}
	}
	if len(p.order) == 1 && p.exits[p.order[0]] != nil && p.exits[p.order[0]].GetExitCode() > 0 {
		return int(p.exits[p.order[0]].GetExitCode())
	}
	if failed {
		return 1
	}
	return 0
}

func dim(color bool, s string) string {
	if !color {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}
