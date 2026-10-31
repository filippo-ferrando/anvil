package ephemeral

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// SetupScript runs once in a warm build, so its clones start with what it installed.
type SetupScript struct {
	Name   string // shown in listings, the script's file name
	Script []byte
	// Files are uploaded into /work for the script and are part of the key: a changed
	// lockfile builds a new snapshot.
	Files []SetupFile
}

type SetupFile struct {
	Name string // relative, slash separated
	Data []byte
}

// maxSetupBytes bounds a setup script and its files together, under gRPC's 4 MiB message.
const maxSetupBytes = 3 << 20

// setupScriptName is the script's name in /work while it runs.
const setupScriptName = ".anvil-setup"

// setupKeyVersion changes the key of every setup snapshot when the way they're built does.
const setupKeyVersion = "setup-v1"

func (s *SetupScript) validate() error {
	if len(bytes.TrimSpace(s.Script)) == 0 {
		return errors.New("the setup script is empty")
	}
	total := len(s.Script)
	seen := map[string]bool{}
	for _, f := range s.Files {
		name := path.Clean(f.Name)
		if f.Name == "" || path.IsAbs(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") || name == setupScriptName {
			return fmt.Errorf("setup file %q: want a relative path inside the project", f.Name)
		}
		if seen[name] {
			return fmt.Errorf("setup file %q given twice", f.Name)
		}
		seen[name] = true
		total += len(f.Data)
	}
	if total > maxSetupBytes {
		return fmt.Errorf("the setup script and its files take %d MiB, more than %d MiB", total>>20, maxSetupBytes>>20)
	}
	return nil
}

// withSetup turns base spec s into the spec of s plus the setup script.
func withSetup(s WarmSpec, setup *SetupScript) (WarmSpec, error) {
	if err := setup.validate(); err != nil {
		return WarmSpec{}, err
	}
	files := append([]SetupFile(nil), setup.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	h := sha256.New()
	parts := []string{setupKeyVersion, s.Key, string(setup.Script)}
	for _, f := range files {
		parts = append(parts, path.Clean(f.Name), string(f.Data))
	}
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(p), p)
	}
	s.Base, s.SetupName, s.Setup = s.Key, setup.Name, setup
	s.Key = hex.EncodeToString(h.Sum(nil))[:16]
	return s, nil
}

// setupTar packs the script and its files for the upload into /work.
func setupTar(setup *SetupScript) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add(setupScriptName, 0o755, setup.Script); err != nil {
		return nil, err
	}
	for _, f := range setup.Files {
		if err := add(path.Clean(f.Name), 0o644, f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// setupTailBytes is how much of a failed setup's output its error shows.
const setupTailBytes = 2 << 10

// scriptSetup runs s's setup script in a warm build VM, from /work, then removes /work so
// the clones' own uploads start clean. A shebang picks the interpreter, sh otherwise.
func scriptSetup(s WarmSpec) Setup {
	return func(ctx context.Context, dial Dialer, progress func(string)) error {
		progress("running the setup script " + s.SetupName)
		upload, err := setupTar(s.Setup)
		if err != nil {
			return err
		}
		conn, err := dial(ctx)
		if err != nil {
			return err
		}
		var out tailBuffer
		script := `./` + setupScriptName + `; rc=$?; cd / && rm -rf "$1"; exit $rc`
		code, err := guestexec.Run(ctx, conn, guestexec.Request{
			Header: guestexec.Header{Argv: []string{"sh", "-c", script, "sh", workDir}, Cwd: workDir},
			Upload: bytes.NewReader(upload), Stdout: &out, Stderr: &out,
		})
		if err != nil {
			return fmt.Errorf("the setup script: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("the setup script %s exited %d: %s", s.SetupName, code, strings.TrimSpace(out.String()))
		}
		return nil
	}
}

// tailBuffer keeps the last setupTailBytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if n := len(t.b); n > setupTailBytes {
		t.b = append([]byte(nil), t.b[n-setupTailBytes:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// warmRef is a distro argument naming one warm snapshot, "@<key or a unique start>".
func warmRef(distro string) (string, bool) {
	return strings.CutPrefix(distro, "@")
}

// specOf is the spec a recorded warm snapshot was built from, for one picked by key.
func specOf(w store.WarmSnapshot) WarmSpec {
	return WarmSpec{
		Key: w.Key, Distro: w.Distro, Arch: w.Arch, CPUs: w.CPUs, MemoryMiB: w.MemoryMiB, DiskGiB: w.DiskGiB,
		FirecrackerVersion: w.FirecrackerVersion, User: w.User, App: w.App, X11: w.X11, Base: w.Base, SetupName: w.Setup,
	}
}

// warmByKey returns the warm snapshot a "@<key>" distro names, which is never built here.
func (m *Manager) warmByKey(prefix string) (store.WarmSnapshot, WarmSpec, error) {
	w, err := m.resolveWarm(prefix)
	if err != nil {
		return store.WarmSnapshot{}, WarmSpec{}, err
	}
	if !m.complete(w.Key) {
		return store.WarmSnapshot{}, WarmSpec{}, fmt.Errorf("warm snapshot %s lost its files, remove it and build it again: %w", w.Key, instance.ErrNotFound)
	}
	return w, specOf(w), nil
}

// SaveClone turns a running or kept clone into a warm snapshot of its own, whose clones
// start where it is now. The clone keeps running.
func (m *Manager) SaveClone(ctx context.Context, prefix string) (store.WarmSnapshot, error) {
	e, err := m.engine()
	if err != nil {
		return store.WarmSnapshot{}, err
	}
	st, err := m.resolveClone(prefix)
	if err != nil {
		return store.WarmSnapshot{}, err
	}
	base, err := m.Store.GetWarm(st.warmKey)
	if err != nil {
		return store.WarmSnapshot{}, fmt.Errorf("the warm snapshot clone %s came from: %w", st.id, err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("saved\n%s\n%s\n%d", base.Key, st.id, time.Now().UnixNano())))
	key := hex.EncodeToString(sum[:])[:16]
	dir := m.Dir(key)
	if err := e.SaveClone(ctx, st.id, m.Dir(base.Key), dir); err != nil {
		return store.WarmSnapshot{}, fmt.Errorf("saving clone %s: %w", st.id, err)
	}
	now := time.Now()
	rec := base
	rec.Key, rec.Base, rec.Setup, rec.Saved = key, base.Key, "saved from "+st.id, true
	rec.SizeBytes, rec.CreatedAt, rec.LastUsedAt = diskUsage(dir), now, now
	if err := m.Store.PutWarm(rec); err != nil {
		return store.WarmSnapshot{}, err
	}
	return rec, nil
}
