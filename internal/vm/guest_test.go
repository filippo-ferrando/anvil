//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

func TestParseCloudInitStatus(t *testing.T) {
	cases := []struct {
		out  string
		want instance.CloudInitStatus
	}{
		{"status: running\n", instance.CloudInitRunning},
		{"\nstatus: done\n", instance.CloudInitDone},
		{"status: degraded done\n", instance.CloudInitDone},
		{"status: error\n", instance.CloudInitError},
		{"status: disabled\n", instance.CloudInitDisabled},
		{"status: not started\n", instance.CloudInitRunning},
		{"", instance.CloudInitUnknown},
		{"Traceback (most recent call last):\n", instance.CloudInitUnknown},
		{"garbage", instance.CloudInitUnknown},
	}
	for _, c := range cases {
		if got := parseCloudInitStatus(c.out); got != c.want {
			t.Errorf("parseCloudInitStatus(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

func TestParseCloudInitResult(t *testing.T) {
	if got := parseCloudInitResult([]byte(`{"v1": {"datasource": "NoCloud", "errors": []}}`)); got != instance.CloudInitDone {
		t.Errorf("got %q, want done", got)
	}
	if got := parseCloudInitResult([]byte(`{"v1": {"errors": ["module failed"]}}`)); got != instance.CloudInitError {
		t.Errorf("got %q, want error", got)
	}
	if got := parseCloudInitResult([]byte(`not json`)); got != instance.CloudInitUnknown {
		t.Errorf("got %q, want unknown", got)
	}
}

func TestGuestAddressesSkipsLoopbackAndLinkLocal(t *testing.T) {
	var ifaces []qemu.GuestInterface
	data := `[{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","prefix":8},{"ip-address":"::1","prefix":128}]},
		{"name":"eth0","ip-addresses":[{"ip-address":"10.55.1.4","prefix":24},{"ip-address":"fe80::5054:ff:fe12:3456","prefix":64},{"ip-address":"fd00::4","prefix":64}]}]`
	if err := json.Unmarshal([]byte(data), &ifaces); err != nil {
		t.Fatal(err)
	}
	got := guestAddresses(ifaces)
	want := []string{"10.55.1.4/24", "fd00::4/64"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestConsoleSaysCloudInitFinished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if consoleSaysCloudInitFinished(path) {
		t.Error("expected false for a missing console log")
	}
	_ = os.WriteFile(path, []byte("[  OK  ] Started cloud-final.service\n"), 0o644)
	if consoleSaysCloudInitFinished(path) {
		t.Error("expected false before the final message")
	}
	_ = os.WriteFile(path, []byte(strings.Repeat("noise\n", 100000)+
		"Cloud-init v. 24.4-0ubuntu1 finished at Tue, 29 Sep 2026 10:00:00 +0000. Datasource DataSourceNoCloud.  Up 31.02 seconds\n"), 0o644)
	if !consoleSaysCloudInitFinished(path) {
		t.Error("expected the final message to be found at the end of a large log")
	}
}

func TestProbeGuestWithoutAgent(t *testing.T) {
	b := NewBackend(nil, nil, noMirrors{})
	agent := qemu.NewGuestAgent(filepath.Join(t.TempDir(), "missing.sock"))

	migrated := &instance.Spec{ID: "01TESTNOSEED", VM: &instance.VMSpec{}}
	info := b.probeGuest(context.Background(), migrated, agent, instance.CloudInitUnknown)
	if info.AgentConnected || info.CloudInit != instance.CloudInitDisabled {
		t.Errorf("expected a seedless VM to report cloud-init disabled, got %+v", info)
	}

	seeded := &instance.Spec{ID: "01TESTSEEDED", VM: &instance.VMSpec{SeedISOPath: "/x/seed.iso"}}
	info = b.probeGuest(context.Background(), seeded, agent, instance.CloudInitUnknown)
	if info.CloudInit != instance.CloudInitUnknown {
		t.Errorf("expected unknown with no agent and no console log, got %+v", info)
	}

	// A final status seen earlier sticks without asking again.
	info = b.probeGuest(context.Background(), seeded, agent, instance.CloudInitError)
	if info.CloudInit != instance.CloudInitError {
		t.Errorf("expected a known final status to be kept, got %+v", info)
	}
}

func TestAddGuestAgent(t *testing.T) {
	existing := "#cloud-config\npackages:\n  - htop\nruncmd:\n  - echo hi\n"
	out := applyTo(t, existing, addGuestAgent)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatalf("output isn't valid YAML: %v\n%s", err, out)
	}
	packages, _ := doc["packages"].([]any)
	if len(packages) != 2 || packages[0] != "htop" || packages[1] != guestAgentPackage {
		t.Errorf("unexpected packages: %v", packages)
	}
	runcmd, _ := doc["runcmd"].([]any)
	if len(runcmd) != 2 || runcmd[0] != "echo hi" || runcmd[1] != guestAgentStartCmd {
		t.Errorf("unexpected runcmd: %v", runcmd)
	}

	// Already listed: not added twice.
	out = applyTo(t, "#cloud-config\npackages: [qemu-guest-agent]\n", addGuestAgent)
	if strings.Count(out, guestAgentPackage+"\n") != 1 {
		t.Errorf("expected the package once, got:\n%s", out)
	}
}

// WaitReady follows the poller's reports instead of probing the guest itself.
func TestWaitReadyFollowsPoller(t *testing.T) {
	b := newGuestTracker(func(string) *qemu.GuestAgent { t.Error("waitReady probed the guest"); return nil }, "powerdown")
	gs := &guestState{cancel: func() {}}
	b.guests["vm1"] = gs
	done := make(chan error, 1)
	go func() {
		done <- b.waitReady(t.Context(), &instance.Spec{ID: "vm1", Name: "vm1"}, nil, nil)
	}()
	// Wait until waitReady is parked on the next report, then publish it.
	for {
		b.guestMu.Lock()
		waiting := gs.probed != nil
		if waiting {
			gs.info = instance.GuestInfo{AgentConnected: true, CloudInit: instance.CloudInitDone}
			gs.notifyProbed()
		}
		b.guestMu.Unlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady didn't return after the poller's report")
	}
}

// A VM without a guest agent never waits for one, so its progress doesn't mention it.
func TestWaitReadyWithoutAgent(t *testing.T) {
	b := newGuestTracker(func(string) *qemu.GuestAgent { return nil }, "powerdown")
	gs := &guestState{cancel: func() {}}
	b.guests["vm1"] = gs
	var statuses []string
	done := make(chan error, 1)
	go func() {
		spec := &instance.Spec{ID: "vm1", Name: "vm1", VM: &instance.VMSpec{NoGuestAgent: true}}
		done <- b.waitReady(t.Context(), spec, nil, func(s string) { statuses = append(statuses, s) })
	}()
	for {
		b.guestMu.Lock()
		waiting := gs.probed != nil
		if waiting {
			gs.info = instance.GuestInfo{CloudInit: instance.CloudInitDone}
			gs.notifyProbed()
		}
		b.guestMu.Unlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0] != "waiting for cloud-init" {
		t.Errorf("progress = %q", statuses)
	}
}

// A VM without the agent is ready on cloud-init's last console line, long before the poller's next round.
func TestWaitReadyReadsConsole(t *testing.T) {
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })
	if err := os.MkdirAll(instanceDir("vm1"), 0o750); err != nil {
		t.Fatal(err)
	}
	b := newGuestTracker(func(string) *qemu.GuestAgent { t.Error("waitReady probed the guest"); return nil }, "powerdown")
	gs := &guestState{cancel: func() {}, wake: make(chan struct{}, 1)}
	b.guests["vm1"] = gs
	spec := &instance.Spec{ID: "vm1", Name: "vm1", VM: &instance.VMSpec{SeedISOPath: "/x/seed.iso", NoGuestAgent: true}}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- b.waitReady(t.Context(), spec, nil, nil) }()
	time.Sleep(100 * time.Millisecond)
	b.guestMu.Lock()
	waiters := gs.waiters
	b.guestMu.Unlock()
	if waiters != 1 || len(gs.wake) != 1 {
		t.Errorf("the poller wasn't told about the wait: %d waiters, %d wakes", waiters, len(gs.wake))
	}
	line := "[  5.1] cloud-init[812]: Cloud-init v. 25.1 finished at Mon, 01 Jan 2026 00:00:00 +0000. Up 5.10 seconds\n"
	if err := os.WriteFile(filepath.Join(instanceDir("vm1"), "console.log"), []byte(line), 0o640); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady missed the console line")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s, want well under the poller's %s", took, guestPollFast)
	}
	if info, _ := b.GuestInfo(spec); info.CloudInit != instance.CloudInitDone {
		t.Errorf("guest info %+v, want cloud-init done", info)
	}
	if gs.waiters != 0 {
		t.Errorf("%d waiters left", gs.waiters)
	}
}

// With the agent up, its report decides: an error isn't hidden by the console's last line.
func TestWaitReadyAgentErrorBeatsConsole(t *testing.T) {
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })
	_ = os.MkdirAll(instanceDir("vm1"), 0o750)
	line := "Cloud-init v. 25.1 finished at Mon, 01 Jan 2026 00:00:00 +0000. Up 5.10 seconds\n"
	_ = os.WriteFile(filepath.Join(instanceDir("vm1"), "console.log"), []byte(line), 0o640)
	b := newGuestTracker(func(string) *qemu.GuestAgent { return nil }, "powerdown")
	gs := &guestState{cancel: func() {}, info: instance.GuestInfo{AgentConnected: true, CloudInit: instance.CloudInitRunning}}
	b.guests["vm1"] = gs
	spec := &instance.Spec{ID: "vm1", Name: "vm1", VM: &instance.VMSpec{SeedISOPath: "/x/seed.iso"}}
	done := make(chan error, 1)
	go func() { done <- b.waitReady(t.Context(), spec, nil, nil) }()
	time.Sleep(3 * consoleCheckEvery)
	for {
		b.guestMu.Lock()
		waiting := gs.probed != nil
		if waiting {
			gs.info.CloudInit = instance.CloudInitError
			gs.notifyProbed()
		}
		b.guestMu.Unlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "errors") {
		t.Errorf("waitReady = %v, want the agent's cloud-init error", err)
	}
}

type countingNetworker struct {
	recordingNetworker
	rx uint64
}

func (n *countingNetworker) Stats(string) (uint64, uint64, error) {
	n.rx += 1000
	return n.rx, 0, nil
}

// A stream's later samples take their rates from the previous one, without a window.
func TestSampleStatsSincePrev(t *testing.T) {
	spec := &instance.Spec{ID: "vm1", VM: &instance.VMSpec{CPUs: 1, MemoryMiB: 64}}
	net := &countingNetworker{}
	pid := os.Getpid()
	start := time.Now()
	st, prev, err := sampleStats(context.Background(), spec, pid, net, true, nil)
	if err != nil || time.Since(start) < statsSampleWindow || !st.NetAvailable || st.NetRxBytesPerSec <= 0 {
		t.Fatalf("first sample = %+v, %v after %s", st, err, time.Since(start))
	}
	time.Sleep(20 * time.Millisecond)
	start = time.Now()
	st, next, err := sampleStats(context.Background(), spec, pid, net, true, prev)
	if took := time.Since(start); err != nil || took > statsSampleWindow/2 {
		t.Fatalf("sample since prev = %v after %s, want no window", err, took)
	}
	if st.NetRxBytesPerSec <= 0 || next.(vmSample).at.Before(prev.(vmSample).at) {
		t.Errorf("rates since prev = %+v", st)
	}
	// A sample of another process (the VM restarted) isn't used.
	other := prev.(vmSample)
	other.pid = pid + 1
	start = time.Now()
	if _, _, err := sampleStats(context.Background(), spec, pid, net, true, other); err != nil || time.Since(start) < statsSampleWindow {
		t.Errorf("a stale sample was used: %v after %s", err, time.Since(start))
	}
}

// A VM with the agent isn't ready on its console alone: the wait holds on until the agent
// reports (so the VM's IP is known), or until agentGrace passes without it.
func TestWaitReadyWaitsForAgent(t *testing.T) {
	root := t.TempDir()
	orig := instanceDir
	instanceDir = func(id string) string { return filepath.Join(root, id) }
	t.Cleanup(func() { instanceDir = orig })
	origGrace := agentGrace
	agentGrace = 600 * time.Millisecond
	t.Cleanup(func() { agentGrace = origGrace })
	line := "Cloud-init v. 25.1 finished at Mon, 01 Jan 2026 00:00:00 +0000. Up 5.10 seconds\n"
	for _, id := range []string{"vm1", "vm2"} {
		_ = os.MkdirAll(instanceDir(id), 0o750)
		_ = os.WriteFile(filepath.Join(instanceDir(id), "console.log"), []byte(line), 0o640)
	}
	b := newGuestTracker(func(string) *qemu.GuestAgent { return nil }, "powerdown")
	wait := func(id string) (chan error, *guestState) {
		gs := &guestState{cancel: func() {}, wake: make(chan struct{}, 1)}
		b.guestMu.Lock()
		b.guests[id] = gs
		b.guestMu.Unlock()
		done := make(chan error, 1)
		spec := &instance.Spec{ID: id, Name: id, VM: &instance.VMSpec{SeedISOPath: "/x/seed.iso"}}
		go func() { done <- b.waitReady(t.Context(), spec, nil, nil) }()
		return done, gs
	}

	// The agent comes up after the console's last line: ready then, with its IP.
	done, gs := wait("vm1")
	select {
	case err := <-done:
		t.Fatalf("ready before the agent: %v", err)
	case <-time.After(3 * consoleCheckEvery):
	}
	start := time.Now()
	for {
		b.guestMu.Lock()
		waiting := gs.probed != nil
		if waiting {
			gs.info = instance.GuestInfo{AgentConnected: true, CloudInit: instance.CloudInitDone, IPAddresses: []string{"10.0.0.5/24"}}
			gs.notifyProbed()
		}
		b.guestMu.Unlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-done; err != nil || time.Since(start) > 300*time.Millisecond {
		t.Errorf("after the agent's report: %v in %s", err, time.Since(start))
	}

	// An agent that never comes: ready once agentGrace passed, as the console said.
	done, _ = wait("vm2")
	start = time.Now()
	select {
	case err := <-done:
		if took := time.Since(start); err != nil || took < agentGrace || took > agentGrace+time.Second {
			t.Errorf("without an agent: %v after %s, want about %s", err, took, agentGrace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never ready without the agent")
	}
}
