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

func TestMergeGuestAgent(t *testing.T) {
	existing := "#cloud-config\npackages:\n  - htop\nruncmd:\n  - echo hi\n"
	out, err := mergeGuestAgent(existing)
	if err != nil {
		t.Fatal(err)
	}
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
	out, _ = mergeGuestAgent("#cloud-config\npackages: [qemu-guest-agent]\n")
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
