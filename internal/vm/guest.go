//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

var (
	_ instance.GuestInspector = (*Backend)(nil)
	_ instance.ReadyWaiter    = (*Backend)(nil)
)

const (
	guestPollFast = 3 * time.Second  // until cloud-init is finished
	guestPollSlow = 30 * time.Second // afterwards, only to follow IP changes
)

// guestState is the backend's view of one running VM's guest.
type guestState struct {
	agent  *qemu.GuestAgent
	info   instance.GuestInfo
	cancel context.CancelFunc
}

func guestAgentSocket(instanceID string) string {
	return filepath.Join(instanceDir(instanceID), "qga.sock")
}

// GuestInfo returns the last guest report for spec, without talking to the guest.
func (b *Backend) GuestInfo(spec *instance.Spec) (instance.GuestInfo, bool) {
	b.guestMu.Lock()
	defer b.guestMu.Unlock()
	gs, ok := b.guests[spec.ID]
	if !ok {
		return instance.GuestInfo{}, false
	}
	info := gs.info
	info.IPAddresses = slices.Clone(info.IPAddresses)
	return info, true
}

func (b *Backend) SetGuestChangeHook(hook func(instanceID string)) {
	b.guestMu.Lock()
	b.guestHook = hook
	b.guestMu.Unlock()
}

// agentFor returns spec's guest agent handle, shared by the poller, Stop and WaitReady.
func (b *Backend) agentFor(instanceID string) *qemu.GuestAgent {
	b.guestMu.Lock()
	defer b.guestMu.Unlock()
	if gs, ok := b.guests[instanceID]; ok {
		return gs.agent
	}
	return qemu.NewGuestAgent(guestAgentSocket(instanceID))
}

// watchGuest starts polling spec's guest agent until the VM stops.
func (b *Backend) watchGuest(spec *instance.Spec, proc *qemu.Process) {
	ctx, cancel := context.WithCancel(context.Background())
	b.guestMu.Lock()
	if old, ok := b.guests[spec.ID]; ok {
		old.cancel()
	}
	gs := &guestState{agent: qemu.NewGuestAgent(guestAgentSocket(spec.ID)), cancel: cancel}
	b.guests[spec.ID] = gs
	b.guestMu.Unlock()

	specCopy := *spec
	go func() {
		defer cancel()
		for {
			info := b.refreshGuest(ctx, &specCopy)
			interval := guestPollFast
			if info.AgentConnected && isFinal(info.CloudInit) {
				interval = guestPollSlow
			}
			select {
			case <-ctx.Done():
				return
			case <-proc.Exited():
				// The VM went away without Stop (guest poweroff, crash): drop the stale report.
				b.forgetGuest(specCopy.ID, gs)
				return
			case <-time.After(interval):
			}
		}
	}()
}

// unwatchGuest stops spec's poller and forgets its guest report.
func (b *Backend) unwatchGuest(instanceID string) {
	b.guestMu.Lock()
	gs := b.guests[instanceID]
	b.guestMu.Unlock()
	if gs != nil {
		b.forgetGuest(instanceID, gs)
	}
}

// forgetGuest drops gs, unless a newer Start already replaced it.
func (b *Backend) forgetGuest(instanceID string, gs *guestState) {
	b.guestMu.Lock()
	if b.guests[instanceID] != gs {
		b.guestMu.Unlock()
		return
	}
	delete(b.guests, instanceID)
	hook := b.guestHook
	b.guestMu.Unlock()
	gs.cancel()
	if hook != nil {
		hook(instanceID)
	}
}

// refreshGuest asks the guest for a new report, stores it, and runs the change
// hook if anything differs from the previous one.
func (b *Backend) refreshGuest(ctx context.Context, spec *instance.Spec) instance.GuestInfo {
	b.guestMu.Lock()
	gs, ok := b.guests[spec.ID]
	var prev instance.GuestInfo
	if ok {
		prev = gs.info
	}
	b.guestMu.Unlock()

	info := b.probeGuest(ctx, spec, b.agentFor(spec.ID), prev.CloudInit)
	if ctx.Err() != nil {
		return prev // an interrupted probe says nothing about the guest
	}

	b.guestMu.Lock()
	gs, ok = b.guests[spec.ID]
	changed := ok && !guestInfoEqual(gs.info, info)
	if changed {
		gs.info = info
	}
	hook := b.guestHook
	b.guestMu.Unlock()
	if changed && hook != nil {
		hook(spec.ID)
	}
	return info
}

// probeGuest collects one guest report. known is the last cloud-init status seen:
// once final it is kept instead of asking again.
func (b *Backend) probeGuest(ctx context.Context, spec *instance.Spec, agent *qemu.GuestAgent, known instance.CloudInitStatus) instance.GuestInfo {
	info := instance.GuestInfo{CloudInit: known}
	_ = agent.Do(ctx, func(c *qemu.QGAConn) error {
		info.AgentConnected = true
		if ifaces, err := c.NetworkInterfaces(); err == nil {
			info.IPAddresses = guestAddresses(ifaces)
		}
		if !isFinal(info.CloudInit) {
			info.CloudInit = cloudInitStatus(ctx, c)
		}
		return nil
	})

	if !isFinal(info.CloudInit) {
		switch {
		case spec.VM == nil || spec.VM.SeedISOPath == "":
			// No NoCloud seed (e.g. a migrated disk): cloud-init has nothing to do this boot.
			info.CloudInit = instance.CloudInitDisabled
		case consoleSaysCloudInitFinished(filepath.Join(instanceDir(spec.ID), "console.log")):
			info.CloudInit = instance.CloudInitDone
		}
	}
	return info
}

func isFinal(s instance.CloudInitStatus) bool {
	return s == instance.CloudInitDone || s == instance.CloudInitError || s == instance.CloudInitDisabled
}

func guestInfoEqual(a, b instance.GuestInfo) bool {
	return a.AgentConnected == b.AgentConnected && a.CloudInit == b.CloudInit && slices.Equal(a.IPAddresses, b.IPAddresses)
}

// guestAddresses flattens the agent's interface list into CIDR strings,
// leaving out loopback and link-local addresses.
func guestAddresses(ifaces []qemu.GuestInterface) []string {
	var out []string
	for _, iface := range ifaces {
		for _, a := range iface.IPAddresses {
			addr, err := netip.ParseAddr(a.Address)
			if err != nil || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, fmt.Sprintf("%s/%d", addr, a.Prefix))
		}
	}
	return out
}

// cloudInitStatus asks the guest how far cloud-init is. `cloud-init status` is tried
// first; some distros block guest-exec, so result.json is read as a fallback.
func cloudInitStatus(ctx context.Context, c *qemu.QGAConn) instance.CloudInitStatus {
	if res, err := c.Exec(ctx, "cloud-init", "status"); err == nil {
		if s := parseCloudInitStatus(res.Stdout, res.ExitCode); s != instance.CloudInitUnknown {
			return s
		}
	}
	data, ok, err := c.ReadFile("/run/cloud-init/result.json")
	if err != nil {
		return instance.CloudInitUnknown
	}
	if !ok {
		return instance.CloudInitRunning // result.json only appears once cloud-init is done
	}
	return parseCloudInitResult(data)
}

// parseCloudInitStatus reads the "status: <word>" line `cloud-init status` prints.
func parseCloudInitStatus(out string, exitCode int) instance.CloudInitStatus {
	for _, line := range strings.Split(out, "\n") {
		word, ok := strings.CutPrefix(strings.TrimSpace(line), "status:")
		if !ok {
			continue
		}
		switch strings.TrimSpace(word) {
		case "running", "not started", "not run":
			return instance.CloudInitRunning
		case "done", "degraded done":
			return instance.CloudInitDone
		case "error", "degraded error":
			return instance.CloudInitError
		case "disabled":
			return instance.CloudInitDisabled
		}
	}
	if exitCode == 1 {
		return instance.CloudInitError
	}
	return instance.CloudInitUnknown
}

// parseCloudInitResult reads /run/cloud-init/result.json, written when cloud-init ends.
func parseCloudInitResult(data []byte) instance.CloudInitStatus {
	var res struct {
		V1 struct {
			Errors []any `json:"errors"`
		} `json:"v1"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return instance.CloudInitUnknown
	}
	if len(res.V1.Errors) > 0 {
		return instance.CloudInitError
	}
	return instance.CloudInitDone
}

// cloudInitFinishedRe matches cloud-init's final console line, e.g.
// "Cloud-init v. 24.1 finished at Mon, 01 Jan 2026 ... Up 42.13 seconds".
var cloudInitFinishedRe = regexp.MustCompile(`Cloud-init v\. \S+ finished at`)

// consoleSaysCloudInitFinished checks the tail of the serial console log, the
// only signal left when the guest agent isn't available.
func consoleSaysCloudInitFinished(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	const tail = 256 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		_, _ = f.Seek(-tail, io.SeekEnd)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	return cloudInitFinishedRe.Match(data)
}

// WaitReady blocks until spec's cloud-init finished, reporting progress while it waits.
func (b *Backend) WaitReady(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm: %s is not running", spec.Name)
	}

	last := ""
	for {
		info := b.refreshGuest(ctx, spec)
		switch info.CloudInit {
		case instance.CloudInitDone, instance.CloudInitDisabled:
			return nil
		case instance.CloudInitError:
			return fmt.Errorf("vm: cloud-init on %s finished with errors (see `anvil logs %s`)", spec.Name, spec.Name)
		}

		status := "waiting for cloud-init: guest agent not up yet"
		if info.AgentConnected {
			status = "waiting for cloud-init: running"
			if len(info.IPAddresses) > 0 {
				status += " (" + strings.Join(info.IPAddresses, ", ") + ")"
			}
		}
		if progress != nil && status != last {
			progress(status)
			last = status
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("vm: waiting for %s's cloud-init: %w", spec.Name, ctx.Err())
		case <-proc.Exited():
			return fmt.Errorf("vm: %s stopped while waiting for cloud-init", spec.Name)
		case <-time.After(2 * time.Second):
		}
	}
}

// agentShutdown asks the guest agent to power the guest off and waits up to
// timeout for QEMU to exit. Returns false if the agent isn't there or the guest didn't stop.
func (b *Backend) agentShutdown(ctx context.Context, instanceID string, proc *qemu.Process, timeout time.Duration) bool {
	err := b.agentFor(instanceID).Do(ctx, func(c *qemu.QGAConn) error { return c.Shutdown() })
	if err != nil {
		return false
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-proc.Exited():
		return true
	case <-waitCtx.Done():
		return false
	}
}
