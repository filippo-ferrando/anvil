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
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

var (
	_ instance.GuestInspector = (*Backend)(nil)
	_ instance.ReadyWaiter    = (*Backend)(nil)
)

const (
	guestPollFast = 3 * time.Second  // until cloud-init is finished
	guestPollSlow = 30 * time.Second // afterwards, only to follow IP changes
	// guestPollWaiting is the pace while someone waits for the guest to be ready.
	guestPollWaiting = 500 * time.Millisecond
	// consoleCheckEvery is how often a wait reads the console log for cloud-init's last line.
	consoleCheckEvery = 250 * time.Millisecond
)

// agentGrace is how long a VM with a guest agent may show cloud-init done (on its console)
// before it counts as ready without the agent: the agent starts at the very end of
// cloud-init, and a wait that returns before it reports leaves the VM with no known IP.
var agentGrace = guestPollFast

// guestTracker polls each running VM's guest agent and keeps its last report. Both
// hypervisor backends embed it; only how the agent is reached differs.
type guestTracker struct {
	guestMu   sync.Mutex
	guests    map[string]*guestState // instance ID -> guest agent poller and last report
	guestHook func(instanceID string)

	// newAgent returns the agent handle for an instance: a unix socket for QEMU, vsock for Firecracker.
	newAgent func(instanceID string) *qemu.GuestAgent
	// shutdownMode is the guest-shutdown mode that ends the VM ("powerdown", or "reboot" on Firecracker).
	shutdownMode string
}

func newGuestTracker(newAgent func(instanceID string) *qemu.GuestAgent, shutdownMode string) guestTracker {
	return guestTracker{guests: make(map[string]*guestState), newAgent: newAgent, shutdownMode: shutdownMode}
}

// guestState is the backend's view of one running VM's guest.
type guestState struct {
	agent   *qemu.GuestAgent
	info    instance.GuestInfo
	cancel  context.CancelFunc
	probed  chan struct{} // closed after the next stored report, made on demand by guestReport
	waiters int           // WaitReady calls following this poller; it polls faster for them
	wake    chan struct{} // cuts the poller's current wait short
}

// notifyProbed wakes everyone waiting for gs's next report. Called with guestMu held.
func (gs *guestState) notifyProbed() {
	if gs.probed != nil {
		close(gs.probed)
		gs.probed = nil
	}
}

func guestAgentSocket(instanceID string) string {
	return filepath.Join(instanceDir(instanceID), "qga.sock")
}

// GuestInfo returns the last guest report for spec, without talking to the guest.
func (b *guestTracker) GuestInfo(spec *instance.Spec) (instance.GuestInfo, bool) {
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

func (b *guestTracker) SetGuestChangeHook(hook func(instanceID string)) {
	b.guestMu.Lock()
	b.guestHook = hook
	b.guestMu.Unlock()
}

// agentFor returns spec's guest agent handle, shared by the poller, Stop and WaitReady.
func (b *guestTracker) agentFor(instanceID string) *qemu.GuestAgent {
	b.guestMu.Lock()
	defer b.guestMu.Unlock()
	if gs, ok := b.guests[instanceID]; ok {
		return gs.agent
	}
	return b.newAgent(instanceID)
}

// watchGuest starts polling spec's guest agent until the VM stops or exited closes.
func (b *guestTracker) watchGuest(spec *instance.Spec, exited <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	b.guestMu.Lock()
	if old, ok := b.guests[spec.ID]; ok {
		old.cancel()
	}
	gs := &guestState{agent: b.newAgent(spec.ID), cancel: cancel, wake: make(chan struct{}, 1)}
	b.guests[spec.ID] = gs
	b.guestMu.Unlock()

	// The poller gets its own VMSpec: Mount rewrites fields such as SeedISOPath on the original.
	specCopy := *spec
	if spec.VM != nil {
		vm := *spec.VM
		specCopy.VM = &vm
	}
	go func() {
		defer cancel()
		for {
			info := b.refreshGuest(ctx, &specCopy)
			interval := guestPollFast
			b.guestMu.Lock()
			waited := gs.waiters > 0
			b.guestMu.Unlock()
			noAgent := specCopy.VM != nil && specCopy.VM.NoGuestAgent
			switch {
			case (info.AgentConnected || noAgent) && isFinal(info.CloudInit):
				interval = guestPollSlow // only IP changes left to follow, or nothing at all without an agent
			case waited:
				interval = guestPollWaiting
			}
			select {
			case <-ctx.Done():
				return
			case <-exited:
				// The VM went away without Stop (guest poweroff, crash): drop the stale report.
				b.forgetGuest(specCopy.ID, gs)
				return
			case <-gs.wake:
			case <-time.After(interval):
			}
		}
	}()
}

// unwatchGuest stops spec's poller and forgets its guest report.
func (b *guestTracker) unwatchGuest(instanceID string) {
	b.guestMu.Lock()
	gs := b.guests[instanceID]
	b.guestMu.Unlock()
	if gs != nil {
		b.forgetGuest(instanceID, gs)
	}
}

// forgetGuest drops gs, unless a newer Start already replaced it.
func (b *guestTracker) forgetGuest(instanceID string, gs *guestState) {
	b.guestMu.Lock()
	if b.guests[instanceID] != gs {
		b.guestMu.Unlock()
		return
	}
	delete(b.guests, instanceID)
	gs.notifyProbed()
	hook := b.guestHook
	b.guestMu.Unlock()
	gs.cancel()
	if hook != nil {
		hook(instanceID)
	}
}

// refreshGuest asks the guest for a new report, stores it, and runs the change
// hook if anything differs from the previous one.
func (b *guestTracker) refreshGuest(ctx context.Context, spec *instance.Spec) instance.GuestInfo {
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
	if ok {
		gs.notifyProbed()
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
func (b *guestTracker) probeGuest(ctx context.Context, spec *instance.Spec, agent *qemu.GuestAgent, known instance.CloudInitStatus) instance.GuestInfo {
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
		if s := parseCloudInitStatus(res.Stdout); s != instance.CloudInitUnknown {
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
func parseCloudInitStatus(out string) instance.CloudInitStatus {
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
	// No status line means the command itself broke (e.g. python upgraded mid-boot), not
	// cloud-init: stay unknown so result.json decides instead of a sticky error.
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
	proc, ok := b.lookup(spec.ID)
	if !ok {
		return fmt.Errorf("vm: %s is not running", spec.Name)
	}
	return b.waitReady(ctx, spec, proc.Exited(), progress)
}

// guestReport returns the poller's last report and a channel closed after its next one,
// or ok false when no poller runs for instanceID.
func (b *guestTracker) guestReport(instanceID string) (info instance.GuestInfo, next <-chan struct{}, ok bool) {
	b.guestMu.Lock()
	defer b.guestMu.Unlock()
	gs, ok := b.guests[instanceID]
	if !ok {
		return instance.GuestInfo{}, nil, false
	}
	if gs.probed == nil {
		gs.probed = make(chan struct{})
	}
	return gs.info, gs.probed, true
}

// followGuest makes instanceID's poller poll faster until the returned func is called.
func (b *guestTracker) followGuest(instanceID string) (done func()) {
	b.guestMu.Lock()
	gs, ok := b.guests[instanceID]
	if ok {
		gs.waiters++
	}
	b.guestMu.Unlock()
	if !ok {
		return func() {}
	}
	select {
	case gs.wake <- struct{}{}:
	default:
	}
	return func() {
		b.guestMu.Lock()
		gs.waiters--
		b.guestMu.Unlock()
	}
}

// consoleDone records that the console log shows cloud-init finished, as the poller would.
func (b *guestTracker) consoleDone(instanceID string) {
	b.guestMu.Lock()
	gs, ok := b.guests[instanceID]
	changed := ok && !isFinal(gs.info.CloudInit)
	if changed {
		gs.info.CloudInit = instance.CloudInitDone
	}
	hook := b.guestHook
	b.guestMu.Unlock()
	if changed && hook != nil {
		hook(instanceID)
	}
}

// waitReady waits until cloud-init is final or exited closes. It follows the guest
// poller's reports, and only probes the guest itself when no poller runs. Between
// reports it reads the console log, which shows the end of cloud-init without the agent.
func (b *guestTracker) waitReady(ctx context.Context, spec *instance.Spec, exited <-chan struct{}, progress func(status string)) error {
	defer b.followGuest(spec.ID)()
	console := ""
	if spec.VM != nil && spec.VM.SeedISOPath != "" {
		console = filepath.Join(instanceDir(spec.ID), "console.log")
	}
	tick := time.NewTicker(consoleCheckEvery)
	defer tick.Stop()
	expectsAgent := spec.VM != nil && !spec.VM.NoGuestAgent
	var doneSince time.Time // cloud-init seen done while the agent isn't up yet
	readyWithoutAgent := func() bool {
		if doneSince.IsZero() {
			doneSince = time.Now()
		}
		return !expectsAgent || time.Since(doneSince) >= agentGrace
	}
	last := ""
	quiet := false // the poller missed a round, so probe directly
	for {
		info, next, polled := b.guestReport(spec.ID)
		if !polled || quiet {
			info = b.refreshGuest(ctx, spec)
		}
		switch info.CloudInit {
		case instance.CloudInitDisabled:
			return nil
		case instance.CloudInitDone:
			if info.AgentConnected || readyWithoutAgent() {
				return nil
			}
		case instance.CloudInitError:
			return fmt.Errorf("vm: cloud-init on %s finished with errors (see `anvil logs %s`)", spec.Name, spec.Name)
		}

		status := "waiting for cloud-init: guest agent not up yet"
		if !expectsAgent {
			status = "waiting for cloud-init"
		} else if info.CloudInit == instance.CloudInitDone && !info.AgentConnected {
			status = "cloud-init done, waiting for the guest agent"
		} else if info.AgentConnected {
			status = "waiting for cloud-init: running"
			if len(info.IPAddresses) > 0 {
				status += " (" + strings.Join(info.IPAddresses, ", ") + ")"
			}
		}
		if progress != nil && status != last {
			progress(status)
			last = status
		}

		wait := 2 * time.Second
		if polled {
			wait = 2 * guestPollFast
		}
		timeout := time.After(wait)
	waiting:
		for {
			select {
			case <-ctx.Done():
				return fmt.Errorf("vm: waiting for %s's cloud-init: %w", spec.Name, ctx.Err())
			case <-exited:
				return fmt.Errorf("vm: %s stopped while waiting for cloud-init", spec.Name)
			case <-next:
				quiet = false
				break waiting
			case <-timeout:
				quiet = polled
				break waiting
			case <-tick.C:
				// Only without the agent, as probeGuest does: a connected agent also reports errors.
				if console != "" && !info.AgentConnected && consoleSaysCloudInitFinished(console) {
					b.consoleDone(spec.ID)
					if readyWithoutAgent() {
						return nil
					}
				}
			}
		}
	}
}

// agentShutdown asks the guest agent to shut the guest down and waits up to timeout
// for exited to close. Returns false if the agent isn't there or the guest didn't stop.
func (b *guestTracker) agentShutdown(ctx context.Context, instanceID string, exited <-chan struct{}, timeout time.Duration) bool {
	err := b.agentFor(instanceID).Do(ctx, func(c *qemu.QGAConn) error { return c.Shutdown(b.shutdownMode) })
	if err != nil {
		return false
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-exited:
		return true
	case <-waitCtx.Done():
		return false
	}
}
