package ephemeral

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
)

// Collect is a chunk of the tar of one collect path from one clone. The last one for an
// index has Done set, and Warning when nothing was sent back.
type Collect struct {
	CloneID, Distro string
	Index           int
	Data            []byte
	Done            bool
	Warning         string
}

// collectChunk is how much tar one Collect event carries at most.
const collectChunk = 256 << 10

// maxCollectList bounds the guest's `find` output for one glob.
const maxCollectList = 16 << 20

// maxCollectArgs bounds the matched paths passed to tar, under the guest's ARG_MAX.
const maxCollectArgs = 1 << 20

// collectPath is one collect path resolved against the command's directory.
type collectPath struct {
	base  string   // absolute directory tar runs in
	glob  []string // the pattern's components after base; nil for a plain path
	names []string // with no glob: the one entry under base
}

// parseCollect resolves p against cwd. A plain path is collected as its last element; a
// glob relative to the directory before its first wildcard.
func parseCollect(p, cwd string) (collectPath, error) {
	if strings.TrimSpace(p) == "" {
		return collectPath{}, fmt.Errorf("empty collect path")
	}
	abs := p
	if !path.IsAbs(abs) {
		abs = path.Join(cwd, abs)
	}
	abs = path.Clean(abs)
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for i, part := range parts {
		if !hasMeta(part) {
			continue
		}
		for _, g := range parts[i:] {
			if _, err := path.Match(g, ""); err != nil {
				return collectPath{}, fmt.Errorf("collect path %s: bad pattern %q", p, g)
			}
		}
		return collectPath{base: "/" + path.Join(parts[:i]...), glob: parts[i:]}, nil
	}
	if abs == "/" {
		return collectPath{}, fmt.Errorf("collect path %s: refusing to collect the whole filesystem", p)
	}
	return collectPath{base: path.Dir(abs), names: []string{path.Base(abs)}}, nil
}

func hasMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }

// matchGlob reports whether the components of name match the pattern components, where
// "**" matches any number of components, none included.
func matchGlob(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchGlob(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := path.Match(pattern[0], name[0])
	return ok && matchGlob(pattern[1:], name[1:])
}

// matchListing picks from find's NUL-separated output under base the entries the glob
// matches, relative to base, without those inside another match (its tar has them).
func matchListing(base string, glob []string, listing []byte) []string {
	prefix := strings.TrimSuffix(base, "/") + "/"
	var matched []string
	for _, p := range bytes.Split(listing, []byte{0}) {
		full := path.Clean(string(p))
		rel, ok := strings.CutPrefix(full, prefix)
		if !ok || rel == "" {
			continue
		}
		if matchGlob(glob, strings.Split(rel, "/")) {
			matched = append(matched, rel)
		}
	}
	sort.Strings(matched)
	var out []string
	for _, m := range matched {
		if n := len(out); n > 0 && strings.HasPrefix(m, out[n-1]+"/") {
			continue
		}
		out = append(out, m)
	}
	return out
}

// collect sends back the tar of every collect path from clone, one after the other.
func (m *Manager) collect(ctx context.Context, clone Clone, distro, cwd string, paths []string, emit func(Collect)) {
	for i, p := range paths {
		if ctx.Err() != nil {
			return
		}
		c := Collect{CloneID: clone.ID(), Distro: distro, Index: i, Done: true}
		if warn := m.collectOne(ctx, clone, cwd, p, func(data []byte) {
			emit(Collect{CloneID: clone.ID(), Distro: distro, Index: i, Data: data})
		}); warn != "" {
			c.Warning = p + ": " + warn
		}
		emit(c)
	}
}

// collectOne streams the tar of one collect path to send and returns a warning when
// nothing, or not all of it, came back.
func (m *Manager) collectOne(ctx context.Context, clone Clone, cwd, p string, send func([]byte)) string {
	cp, err := parseCollect(p, cwd)
	if err != nil {
		return err.Error()
	}
	if cp.glob != nil {
		var listing limitedBuffer
		listing.max = maxCollectList
		var stderr strings.Builder
		// A missing base is just no match; find's exit code says nothing more.
		_, err := m.guestRun(ctx, clone, guestexec.Request{
			Header: guestexec.Header{Argv: []string{"find", strings.TrimSuffix(cp.base, "/") + "/", "-print0"}},
			Stdout: &listing, Stderr: &stderr,
		})
		if err != nil {
			return "listing: " + err.Error()
		}
		if listing.over {
			return "too many files under " + cp.base
		}
		cp.names = matchListing(cp.base, cp.glob, listing.Bytes())
		if len(cp.names) == 0 {
			return "no match"
		}
	}
	argv := []string{"tar", "-C", cp.base, "-cf", "-"}
	size := 0
	for _, n := range cp.names {
		argv = append(argv, "./"+n) // never read as an option
		size += len(n) + 3
	}
	if size > maxCollectArgs {
		return fmt.Sprintf("%d matches, too many for one tar; collect their directory instead", len(cp.names))
	}
	if cp.glob == nil {
		// One shell, so a missing path costs no extra round trip.
		argv = append([]string{"sh", "-c", `test -e "$1" || test -L "$1" || exit 3; shift; exec "$@"`, "sh", path.Join(cp.base, cp.names[0])}, argv...)
	}
	w := &chunker{send: send}
	var stderr strings.Builder
	code, err := m.guestRun(ctx, clone, guestexec.Request{Header: guestexec.Header{Argv: argv}, Stdout: w, Stderr: &stderr})
	w.flush()
	switch {
	case err != nil:
		return err.Error()
	case code == 3 && cp.glob == nil && w.sent == 0:
		return "no such file"
	case code != 0:
		return fmt.Sprintf("tar exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return ""
}

// chunker hands out what it is written in collectChunk pieces, each its own slice.
type chunker struct {
	buf  []byte
	sent int
	send func([]byte)
}

func (c *chunker) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for len(c.buf) >= collectChunk {
		c.emit(c.buf[:collectChunk:collectChunk])
		c.buf = append([]byte(nil), c.buf[collectChunk:]...)
	}
	return len(p), nil
}

func (c *chunker) flush() {
	if len(c.buf) > 0 {
		c.emit(c.buf)
		c.buf = nil
	}
}

func (c *chunker) emit(b []byte) {
	c.sent += len(b)
	c.send(b)
}

// limitedBuffer keeps up to max bytes and notes when more came.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.over = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
