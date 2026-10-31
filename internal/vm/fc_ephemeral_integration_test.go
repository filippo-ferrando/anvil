//go:build linux

package vm

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
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
// realCloneManager is an ephemeral manager over a real FCBackend, its clones unjailed in
// their own netns, with a DNS server on their links. It skips outside `unshare -rnm`.
func realCloneManager(t *testing.T) (*FCBackend, *ephemeral.Manager, string) {
	t.Helper()
	data, _ := os.ReadFile("/proc/self/uid_map")
	if strings.Join(strings.Fields(string(data)), " ") == "0 0 4294967295" || os.Geteuid() != 0 {
		t.Skip("needs its own user, network and mount namespaces, run under unshare -rnm")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	b, root := newFCTestBackend(t)
	b.unjailedClones = true
	t.Setenv(netnsPoolEnv, "0") // the test owns every netns it sees
	hostdev.NetnsDir = filepath.Join(root, "netns")
	t.Cleanup(func() { hostdev.NetnsDir = "/run/netns" })
	b.Clones.Helper = hostdev.LocalNetns{UID: os.Getuid(), GID: os.Getgid()}
	t.Setenv("ANVIL_GUEST_TOOLS", buildAnvilGuest(t))
	db, err := store.Open(filepath.Join(root, "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := ephemeral.NewManager(b, db, func(key string) string { return filepath.Join(root, "warm", key) })

	// anvild's DNS server, on the host end of each clone's link as clones come and go.
	srv := dns.NewServer(func(context.Context) ([]dns.Zone, error) { return b.Clones.DNSZones(), nil })
	t.Cleanup(func() { srv.Close() })
	b.Clones.OnChange = func() { _ = srv.Refresh(context.Background()) }
	return b, m, root
}

func TestEphemeralRealRun(t *testing.T) {
	_, m, root := realCloneManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	probe := `uname -sr; ip -4 -o addr show eth0 | awk '{print $4}'; cat /proc/sys/kernel/random/boot_id; hostname; grep -qw $(hostname) /etc/hosts && echo resolves; ` +
		`(timeout 5 bash -c 'exec 3<>/dev/tcp/10.200.0.1/53' 2>/dev/null || timeout 5 nc -z 10.200.0.1 53) && echo dns-reachable; ` +
		// A bound driver dir lists its devices next to bind, unbind, uevent and module.
		`ls /sys/bus/virtio/drivers/virtio_balloon/ 2>/dev/null | grep -qvE '^(bind|unbind|uevent|module)$' && echo balloon; ` +
		`ls /sys/bus/*/drivers/vmgenid/ 2>/dev/null | grep -qvE '^(bind|unbind|uevent|module|)$|:$' && echo vmgenid; ` +
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
		// Free page reporting and the RNG reseed on restore need both devices bound.
		if !strings.Contains(out, "\nballoon\n") || !strings.Contains(out, "\nvmgenid\n") {
			t.Errorf("clone %s: the balloon or VMGenID device isn't bound: %q", id, out)
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
	m.WaitDeletes()

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

// TestEphemeralRealRunUffd restores clones with ANVIL_EPH_MEMORY=uffd: the first records
// the working set, the next one prefetches it. Firecracker needs a userfaultfd, which an
// unprivileged process only gets with vm.unprivileged_userfaultfd=1.
func TestEphemeralRealRunUffd(t *testing.T) {
	if data, _ := os.ReadFile("/proc/sys/vm/unprivileged_userfaultfd"); strings.TrimSpace(string(data)) != "1" {
		t.Skip("needs vm.unprivileged_userfaultfd=1 for an unjailed firecracker")
	}
	_, m, root := realCloneManager(t)
	t.Setenv(memoryEnv, "uffd")
	orig := workingSetWindow
	workingSetWindow = 3 * time.Second
	t.Cleanup(func() { workingSetWindow = orig })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var progress []string
	run := func() ephemeral.Exit {
		t.Helper()
		var exit ephemeral.Exit
		err := m.Run(ctx, ephemeral.RunRequest{Distros: []string{"e2e"}, Argv: []string{"sh", "-c", "uname -sr; sleep 4"}}, func(ev ephemeral.Event) {
			switch {
			case ev.Progress != nil:
				progress = append(progress, ev.Progress.Message)
				t.Logf("%s", ev.Progress.Message)
			case ev.Exit != nil:
				exit = *ev.Exit
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		m.WaitDeletes() // the first clone saves its working set as it goes
		return exit
	}
	if ex := run(); ex.Code != 0 {
		t.Fatalf("first uffd clone: %+v", ex)
	}
	warm, _ := filepath.Glob(filepath.Join(root, "warm", "*", warmWorkingSet))
	if len(warm) != 1 {
		t.Fatalf("no working set recorded: %v", warm)
	}
	pages, err := firecracker.ReadWorkingSet(warm[0])
	if err != nil || len(pages) == 0 {
		t.Fatalf("working set = %d pages, %v", len(pages), err)
	}
	t.Logf("working set: %d pages", len(pages))
	progress = nil
	if ex := run(); ex.Code != 0 {
		t.Fatalf("second uffd clone: %+v", ex)
	}
	prefetched := false
	for _, p := range progress {
		prefetched = prefetched || (strings.Contains(p, "prefetch ") && !strings.Contains(p, "prefetch 0 pages"))
	}
	if !prefetched {
		t.Errorf("the second clone prefetched nothing: %q", progress)
	}
}

// TestEphemeralRealPool fills a pool with a real clone and runs a command on it.
func TestEphemeralRealPool(t *testing.T) {
	_, m, _ := realCloneManager(t)
	if _, err := m.SetPool(ephemeral.WarmParams{Distro: "e2e"}, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		pools, err := m.Pools()
		if err != nil {
			t.Fatal(err)
		}
		if len(pools) == 1 && pools[0].Ready == 1 {
			break
		}
		if len(pools) == 1 && pools[0].Err != "" {
			t.Fatalf("filling the pool: %s", pools[0].Err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pool never filled: %+v", pools)
		}
		time.Sleep(200 * time.Millisecond)
	}
	var progress []string
	var exit ephemeral.Exit
	var out strings.Builder
	start := time.Now()
	err := m.Run(context.Background(), ephemeral.RunRequest{Distros: []string{"e2e"}, Argv: []string{"sh", "-c", "hostname; date +%s"}},
		func(ev ephemeral.Event) {
			switch {
			case ev.Progress != nil:
				progress = append(progress, ev.Progress.Message)
			case ev.Output != nil:
				out.Write(ev.Output.Data)
			case ev.Exit != nil:
				exit = *ev.Exit
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("run on a pooled clone took %s: %q %q", time.Since(start).Round(time.Millisecond), progress, out.String())
	lines := strings.Fields(out.String())
	if exit.Code != 0 || !strings.HasPrefix(exit.CloneID, "eph-P") || len(lines) != 2 || lines[0] != strings.ToLower(exit.CloneID) {
		t.Fatalf("pooled run: exit %+v, output %q", exit, out.String())
	}
	// The clock was set when the clone was handed out.
	if sec, _ := strconv.ParseInt(lines[1], 10, 64); time.Since(time.Unix(sec, 0)).Abs() > 5*time.Second {
		t.Errorf("guest clock %s, host %s", time.Unix(sec, 0), time.Now())
	}
	if _, err := m.SetPool(ephemeral.WarmParams{Distro: "e2e"}, 0); err != nil {
		t.Fatal(err)
	}
}
