package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/status"

	"git.fferrando.cc/rdfilippo/anvil/internal/gui/events"
)

const logPoll = 200 * time.Millisecond

// followGUICalls runs the anvil CLI once per call and hands over each event as it comes,
// then follows the detached run's log until the app runs.
func followGUICalls(ctx context.Context, calls [][]string, wrap func(lineStep) tea.Msg) tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return wrap(failed[struct{}](err)) }
	}
	return followCalls(ctx, exe, calls, wrap)
}

// followCalls ends with a Canceled error once ctx ends, even if the calls went on.
func followCalls(ctx context.Context, exe string, calls [][]string, wrap func(lineStep) tea.Msg) tea.Cmd {
	lines, result := make(chan string), make(chan error, 1)
	// Once ctx ends lines are dropped: the calls still finish, nobody watches them.
	emit := func(l string) {
		select {
		case lines <- l:
		case <-ctx.Done():
		}
	}
	go func() { result <- runFollowed(ctx, exe, calls, emit) }()
	var next tea.Cmd
	next = func() tea.Msg {
		select {
		case l := <-lines:
			return wrap(lineStep{status: l, next: next})
		case err := <-result:
			if err == nil && ctx.Err() != nil {
				err = status.FromContextError(ctx.Err()).Err()
			}
			if err != nil {
				return wrap(failed[struct{}](err))
			}
			return wrap(lineStep{done: true})
		}
	}
	return next
}

// runFollowed runs `anvil gui --json` calls in order, stopping at the first failure, then
// follows the events in the log of a detached call.
func runFollowed(ctx context.Context, exe string, calls [][]string, emit func(string)) error {
	var detached *events.Event
	for _, args := range calls {
		if ctx.Err() != nil {
			return nil // followCalls reports the cancel
		}
		d, err := runStreamed(ctx, exe, args, emit)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if d != nil {
			detached = d
		}
	}
	if detached == nil || ctx.Err() != nil {
		return nil
	}
	return followGUILog(ctx, detached.Log, detached.Offset, emit)
}

// runStreamed runs exe with args, emitting each event on its stdout, and returns the
// detached event if there was one. ctx kills the call, except a --detach one.
func runStreamed(ctx context.Context, exe string, args []string, emit func(string)) (*events.Event, error) {
	cmd := exec.Command(exe, args...)
	if !slices.Contains(args, "--detach") {
		cmd = exec.CommandContext(ctx, exe, args...)
		cmd.WaitDelay = time.Second
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var detached *events.Event
	failure := ""
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		e, ok := events.Parse(sc.Bytes())
		if !ok {
			continue
		}
		switch e.Event {
		case events.Error:
			failure = e.Message
			continue
		case events.Done:
			continue
		case events.Detached:
			detached = &e
		}
		emit(e.String())
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	switch {
	case err == nil:
		return detached, nil
	case failure != "":
		return nil, errors.New(failure)
	case lastLine(stderr.String()) != "":
		return nil, errors.New(lastLine(stderr.String())) // failed before it could report, e.g. a bad flag
	}
	return nil, err
}

// followGUILog emits the events a detached `anvil gui --json` logs from offset on, until
// the app runs, the run ends or fails, or ctx ends. Other lines are the app's own output.
func followGUILog(ctx context.Context, path string, offset int64, emit func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	var buf []byte
	for {
		more, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		buf = append(buf, more...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			e, ok := events.Parse(buf[:i])
			buf = buf[i+1:]
			if !ok {
				continue
			}
			switch e.Event {
			case events.Running:
				emit(e.String())
				return nil
			case events.Error:
				return errors.New(e.Message)
			case events.Done:
				return nil
			}
			emit(e.String())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logPoll):
		}
	}
}
