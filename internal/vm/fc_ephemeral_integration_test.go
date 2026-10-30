//go:build linux

package vm

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// cloneRun is what one clone of a run reported.
type cloneRun struct {
	out  strings.Builder
	exit ephemeral.Exit
}

func runClones(t *testing.T, ctx context.Context, m *ephemeral.Manager, r ephemeral.RunRequest) map[string]*cloneRun {
	t.Helper()
	var mu sync.Mutex
	got := map[string]*cloneRun{}
	at := func(id string) *cloneRun {
		if got[id] == nil {
			got[id] = &cloneRun{}
		}
		return got[id]
	}
	err := m.Run(ctx, r, func(ev ephemeral.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case ev.Progress != nil:
			t.Logf("[%s] %s", ev.Progress.CloneID, ev.Progress.Message)
		case ev.Output != nil:
			at(ev.Output.CloneID).out.Write(ev.Output.Data)
		case ev.Exit != nil:
			at(ev.Exit.CloneID).exit = *ev.Exit
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got
}

// TestEphemeralRealRun builds a real warm snapshot and runs clones of it through the
// ephemeral manager. Without root the jailer can't run, so the clones run unjailed from
// their own dirs; run it under `unshare -rnm` with ANVIL_E2E_IMAGE and ANVIL_FC_KERNEL.
func TestEphemeralRealRun(t *testing.T) {
	data, _ := os.ReadFile("/proc/self/uid_map")
	if strings.Join(strings.Fields(string(data)), " ") == "0 0 4294967295" || os.Geteuid() != 0 {
		t.Skip("needs its own user, network and mount namespaces, run under unshare -rnm")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	b, root := newFCTestBackend(t)
	b.unjailedClones = true
	hostdev.NetnsDir = filepath.Join(root, "netns")
	t.Cleanup(func() { hostdev.NetnsDir = "/run/netns" })
	b.Clones.Helper = hostdev.LocalNetns{UID: os.Getuid(), GID: os.Getgid()}
	t.Setenv("ANVIL_GUEST_TOOLS", buildAnvilGuest(t))
	db, err := store.Open(filepath.Join(root, "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := ephemeral.NewManager(b, db, func(key string) string { return filepath.Join(root, "warm", key) })

	// anvild's DNS server, on the host end of each clone's link as clones come and go.
	srv := dns.NewServer(func(context.Context) ([]dns.Zone, error) { return b.Clones.DNSZones(), nil })
	defer srv.Close()
	b.Clones.OnChange = func() { _ = srv.Refresh(context.Background()) }

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	probe := `uname -sr; ip -4 -o addr show eth0 | awk '{print $4}'; cat /proc/sys/kernel/random/boot_id; hostname; grep -qw $(hostname) /etc/hosts && echo resolves; ` +
		`(timeout 5 bash -c 'exec 3<>/dev/tcp/10.200.0.1/53' 2>/dev/null || timeout 5 nc -z 10.200.0.1 53) && echo dns-reachable; ` +
		`if test -d /run/systemd/system; then echo systemd; fi`
	start := time.Now()
	got := runClones(t, ctx, m, ephemeral.RunRequest{Distros: []string{"e2e", "e2e"}, Argv: []string{"sh", "-c", probe}})
	t.Logf("first run (warm build included) took %s", time.Since(start).Round(time.Second))
	if len(got) != 2 {
		t.Fatalf("got %d clones, want 2: %v", len(got), got)
	}
	// The kernel makes boot_id on its first read: under systemd that's before the snapshot.
	var bootIDs []string
	for id, r := range got {
		out := r.out.String()
		t.Logf("%s exit %d in %s: %q %s", id, r.exit.Code, r.exit.Duration.Round(time.Millisecond), out, r.exit.Err)
		if r.exit.Code != 0 || !strings.Contains(out, "Linux") || !strings.Contains(out, "10.200.0.2/30") || !strings.Contains(out, "dns-reachable") {
			t.Errorf("clone %s: exit %+v, output %q", id, r.exit, out)
		}
		if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) >= 5 {
			if strings.HasSuffix(out, "systemd\n") {
				bootIDs = append(bootIDs, lines[2])
			}
			if lines[3] != strings.ToLower(id) || lines[4] != "resolves" {
				t.Errorf("clone %s: hostname %q, %q, want its own id resolving", id, lines[3], lines[4])
			}
		}
	}
	if len(bootIDs) == 2 && bootIDs[0] != bootIDs[1] {
		t.Errorf("the clones booted separately (boot ids %v), they should be restores of one snapshot", bootIDs)
	}
	if list, _ := m.ListWarm(); len(list) != 1 {
		t.Fatalf("warm snapshots after two concurrent clones = %+v, want one build", list)
	} else {
		t.Logf("warm snapshot %s: %d bytes on disk", list[0].Key, list[0].SizeBytes)
	}

	// From the warm snapshot: an upload, a failing script kept for a look inside.
	upload := filepath.Join(root, "upload.tar")
	f, err := os.Create(upload)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	script := "#!/bin/sh\necho checking in $(pwd)\nexit 3\n"
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "check.sh", Mode: 0o755, Size: int64(len(script))})
	_, _ = tw.Write([]byte(script))
	_ = tw.Close()
	f.Close()
	start = time.Now()
	got = runClones(t, ctx, m, ephemeral.RunRequest{Distros: []string{"e2e"}, Argv: []string{"./check.sh"}, Upload: upload, KeepFailed: true})
	t.Logf("warm run took %s", time.Since(start).Round(time.Millisecond))
	var kept ephemeral.Exit
	for _, r := range got {
		kept = r.exit
		if r.exit.Code != 3 || !r.exit.Kept || r.out.String() != "checking in /work\n" {
			t.Errorf("failing script: exit %+v, output %q", r.exit, r.out.String())
		}
	}
	var out strings.Builder
	code, err := m.Exec(ctx, kept.CloneID, guestexec.Request{Header: guestexec.Header{Argv: []string{"ls", "/work"}}, Stdout: &out})
	if err != nil || code != 0 || out.String() != "check.sh\n" {
		t.Errorf("Exec in the kept clone = %d, %v, %q", code, err, out.String())
	}
	if _, err := m.DeleteClones(ctx, nil, true); err != nil {
		t.Fatal(err)
	}

	if clones := m.Clones(); len(clones) != 0 {
		t.Errorf("clones left: %+v", clones)
	}
	if entries, _ := os.ReadDir(hostdev.NetnsDir); len(entries) != 0 {
		t.Errorf("netns left: %v", entries)
	}
	if left, _ := filepath.Glob(instanceDir("eph-*")); len(left) != 0 {
		t.Errorf("clone dirs left: %v", left)
	}
}
