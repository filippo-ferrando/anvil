package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/status"
)

const (
	detachedPrefix = "started in the background, log: "
	appRunsMarker  = " runs in " // in the "anvil: <app> runs in <where>" line once the app started
	logPoll        = 200 * time.Millisecond
)

// followGUICalls runs the anvil CLI once per call and hands over each output line as it
// comes, then follows the detached run's log until the app runs.
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

// runFollowed runs calls in order, stopping at the first failure, then follows the log a
// detached call printed.
func runFollowed(ctx context.Context, exe string, calls [][]string, emit func(string)) error {
	logPath := ""
	for _, args := range calls {
		last, err := runStreamed(exe, args, emit)
		if err != nil {
			if last != "" {
				err = errors.New(strings.TrimPrefix(last, "anvil: "))
			}
			return err
		}
		if last != "" {
			emit(last)
		}
		if p, ok := strings.CutPrefix(last, detachedPrefix); ok {
			logPath = strings.TrimSpace(p)
		}
	}
	if logPath == "" || ctx.Err() != nil {
		return nil
	}
	return followGUILog(ctx, logPath, emit)
}

// runStreamed runs exe with args, emitting each output line but the last, which it returns.
func runStreamed(exe string, args []string, emit func(string)) (string, error) {
	r, w := io.Pipe()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		return "", err
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		w.Close()
		waited <- err
	}()
	last := ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" {
			continue
		}
		if last != "" {
			emit(last)
		}
		last = l
	}
	_, _ = io.Copy(io.Discard, r)
	return last, <-waited
}

// followGUILog emits the lines of the last run in a detached `anvil gui` log until the app
// runs, the run fails, or ctx ends.
func followGUILog(ctx context.Context, path string, emit func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	buf := data[lastRunStart(data):]
	for {
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimSpace(string(buf[:i]))
			buf = buf[i+1:]
			if line == "" || strings.HasPrefix(line, "--- ") {
				continue
			}
			text := strings.TrimPrefix(line, "anvil: ")
			switch {
			case text != line && strings.Contains(text, appRunsMarker):
				emit(text)
				return nil
			case text != line:
				return errors.New(text)
			}
			emit(line)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logPoll):
		}
		more, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		buf = append(buf, more...)
	}
}

// lastRunStart finds the "--- " header the last detached run wrote, len(data) for none.
func lastRunStart(data []byte) int {
	if i := bytes.LastIndex(data, []byte("\n--- ")); i >= 0 {
		return i + 1
	}
	if bytes.HasPrefix(data, []byte("--- ")) {
		return 0
	}
	return len(data)
}
