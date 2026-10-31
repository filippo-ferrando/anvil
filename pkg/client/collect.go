package client

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
)

// CollectSpec is one `--collect <guest-path>[:<host-dir>]`.
type CollectSpec struct {
	Guest, Host string
}

// ParseCollect splits `<guest-path>[:<host-dir>]`; the host dir defaults to ".".
func ParseCollect(s string) (CollectSpec, error) {
	guest, host, _ := strings.Cut(s, ":")
	if strings.TrimSpace(guest) == "" {
		return CollectSpec{}, fmt.Errorf("--collect %q: want <guest-path>[:<host-dir>]", s)
	}
	if host == "" {
		host = "."
	}
	return CollectSpec{Guest: guest, Host: host}, nil
}

// CollectDir is where spec's files from distro go: under a directory per distro when
// perDistro is set, as in a matrix.
func CollectDir(spec CollectSpec, distro string, perDistro bool) string {
	if !perDistro {
		return spec.Host
	}
	return filepath.Join(spec.Host, strings.ReplaceAll(distro, "/", "_"))
}

// Collector extracts the RunCollect events of a run, each path of each clone into the
// directory dest picks for it.
type Collector struct {
	paths []string
	dest  func(index int, distro string) string
	open  map[string]*extraction
}

type extraction struct {
	dir  string
	pw   *io.PipeWriter
	done chan error
	err  error
}

// CollectResult is a finished path of one clone: Dir it went into, or Err why not.
type CollectResult struct {
	Distro, Path, Dir string
	Err               error
}

// NewCollector takes the run's collect paths, as sent in RunRequest.collect.
func NewCollector(paths []string, dest func(index int, distro string) string) *Collector {
	return &Collector{paths: paths, dest: dest, open: map[string]*extraction{}}
}

// Handle takes one event and returns a result once its path is done.
func (c *Collector) Handle(ev *anvilv1.RunCollect) *CollectResult {
	key := fmt.Sprintf("%s\x00%d", ev.GetCloneId(), ev.GetIndex())
	x := c.open[key]
	if len(ev.GetData()) > 0 {
		if x == nil {
			x = c.start(int(ev.GetIndex()), ev.GetDistro())
			c.open[key] = x
		}
		if x.err == nil {
			if _, err := x.pw.Write(ev.GetData()); err != nil {
				x.err = err
			}
		}
	}
	if !ev.GetDone() {
		return nil
	}
	delete(c.open, key)
	res := &CollectResult{Distro: ev.GetDistro()}
	if i := int(ev.GetIndex()); i >= 0 && i < len(c.paths) {
		res.Path = c.paths[i]
	}
	if x != nil {
		x.pw.Close()
		if err := <-x.done; x.err == nil {
			x.err = err
		}
		res.Dir, res.Err = x.dir, x.err
	}
	if res.Err == nil && ev.GetWarning() != "" {
		res.Err = fmt.Errorf("%s", ev.GetWarning())
	}
	return res
}

func (c *Collector) start(index int, distro string) *extraction {
	pr, pw := io.Pipe()
	x := &extraction{dir: c.dest(index, distro), pw: pw, done: make(chan error, 1)}
	go func() {
		err := os.MkdirAll(x.dir, 0o755)
		if err == nil {
			err = guestexec.Extract(pr, x.dir)
		}
		if err != nil {
			err = fmt.Errorf("extracting into %s: %w", x.dir, err)
			pr.CloseWithError(err)
		} else {
			_, _ = io.Copy(io.Discard, pr) // the tar's end padding, so the writer never blocks
		}
		x.done <- err
	}()
	return x
}

// Close stops every extraction still going, as when the run broke off.
func (c *Collector) Close() {
	for k, x := range c.open {
		x.pw.CloseWithError(io.ErrUnexpectedEOF)
		<-x.done
		delete(c.open, k)
	}
}
