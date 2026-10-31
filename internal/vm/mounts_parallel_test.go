//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// fakeVirtiofsd puts a virtiofsd on PATH that takes delay to create its socket.
func fakeVirtiofsd(t *testing.T, delay string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do case $a in --socket-path=*) sock=${a#--socket-path=};; esac; done\n" +
		"sleep " + delay + "\n: > \"$sock\"\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "virtiofsd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestStartVirtiofsdsInParallel(t *testing.T) {
	fakeVirtiofsd(t, "0.3")
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })
	if err := os.MkdirAll(instanceDir("vm1"), 0o750); err != nil {
		t.Fatal(err)
	}
	b := NewBackend(nil, nil, noMirrors{})
	spec := &instance.Spec{ID: "vm1", Name: "vm1", VM: &instance.VMSpec{}}
	for _, tag := range []string{"a", "b", "c", "d"} {
		dir := filepath.Join(root, "share-"+tag)
		_ = os.MkdirAll(dir, 0o750)
		spec.VM.Mounts = append(spec.VM.Mounts, instance.Mount{Tag: tag, HostPath: dir})
	}
	start := time.Now()
	mounts, err := b.startVirtiofsds(spec)
	took := time.Since(start)
	t.Cleanup(func() { b.killVirtiofsds("vm1") })
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 4 || mounts[0].Tag != "a" || mounts[3].Tag != "d" {
		t.Errorf("mounts %+v, want a..d in order", mounts)
	}
	if took > 900*time.Millisecond {
		t.Errorf("4 virtiofsd took %s to start, want about one's 300ms", took)
	}

	// A missing shared dir fails before any virtiofsd starts.
	spec.VM.Mounts = append(spec.VM.Mounts, instance.Mount{Tag: "e", HostPath: filepath.Join(root, "missing")})
	if _, err := b.startVirtiofsds(&instance.Spec{ID: "vm2", Name: "vm2", VM: &instance.VMSpec{Mounts: spec.VM.Mounts[4:]}}); err == nil {
		t.Error("a missing shared dir was accepted")
	}
}
