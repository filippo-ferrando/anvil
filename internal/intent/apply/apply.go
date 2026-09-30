package apply

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// Instances is the subset of *instance.Manager's methods Applier needs.
type Instances interface {
	GetByID(id string) (*instance.Spec, error)
	Start(ctx context.Context, names []string) error
	Delete(ctx context.Context, names []string, purge bool) error
	Update(ctx context.Context, name string, p instance.UpdateParams) (instance.UpdateResult, error)
	WaitReady(ctx context.Context, name string, progress func(status string)) error
}

// Intents is the subset of *intent.Manager's methods Applier needs.
type Intents interface {
	Info(name string) (store.Intent, error)
	Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error
	Remove(ctx context.Context, name, member string) (store.Intent, error)
}

// Action is what applying does to one member.
type Action string

const (
	ActionCreate   Action = "create"
	ActionRecreate Action = "recreate"
	ActionUpdate   Action = "update"
	ActionStart    Action = "start"
	ActionKeep     Action = "keep"
	ActionDelete   Action = "delete"
)

// Step is one planned change. Changes says why, one short line per difference.
type Step struct {
	Role    string
	Name    string
	Action  Action
	Changes []string

	current *instance.Spec
	staleID string // member record whose instance is gone, dropped before the create
	update  instance.UpdateParams
	start   bool
}

// Options change how Apply plans and runs.
type Options struct {
	DryRun bool
	// Prune deletes members that are no longer in the manifest; without it they are kept.
	Prune bool
	// Recreate allows replacing a VM, which loses its disk. Containers are always replaced when needed.
	Recreate bool
	// SSHKeys are authorized on every new VM on top of the manifest's own, e.g. the caller's anvil key.
	SSHKeys []string
	// WaitTimeout bounds the readiness wait for each member others depend on.
	WaitTimeout time.Duration
}

const defaultWaitTimeout = 15 * time.Minute

// Applier plans and applies manifests. One apply runs at a time.
type Applier struct {
	Instances Instances
	Intents   Intents
	mu        sync.Mutex
}

// Apply plans m against the current state, reports the plan through onPlan and, unless
// opts.DryRun, carries it out. A VM that needs replacing without opts.Recreate fails the plan.
func (a *Applier) Apply(ctx context.Context, m *Manifest, opts Options, onPlan func([]Step), progress func(string)) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	steps, err := a.plan(m, opts)
	if steps != nil {
		onPlan(steps)
	}
	if err != nil || opts.DryRun {
		return err
	}
	for _, s := range steps {
		if err := a.run(ctx, m, s, opts, progress); err != nil {
			return fmt.Errorf("apply: %s (%s): %w", s.Role, s.Action, err)
		}
	}
	return nil
}

func (a *Applier) plan(m *Manifest, opts Options) ([]Step, error) {
	current := map[string]*instance.Spec{}
	stale := map[string]string{}
	it, err := a.Intents.Info(m.Intent)
	switch {
	case errors.Is(err, instance.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		for _, mem := range it.Members {
			spec, err := a.Instances.GetByID(mem.InstanceID)
			if err != nil || spec.State == instance.StateDeleted {
				stale[mem.Role] = mem.InstanceID
				continue
			}
			current[mem.Role] = spec
		}
	}

	// Deletes go first, so a replacement can reuse a name or host port.
	var steps []Step
	var orphans []string
	for role := range current {
		if _, ok := m.Members[role]; !ok {
			orphans = append(orphans, role)
		}
	}
	sort.Strings(orphans)
	for _, role := range orphans {
		s := Step{Role: role, Name: current[role].Name, current: current[role], Action: ActionDelete, Changes: []string{"not in the manifest"}}
		if !opts.Prune {
			s.Action = ActionKeep
			s.Changes = []string{"not in the manifest, kept (prune deletes it)"}
		}
		steps = append(steps, s)
	}

	order, err := m.Order()
	if err != nil {
		return nil, err
	}
	var blocked []string
	for _, role := range order {
		mem := m.Members[role]
		cur := current[role]
		s := Step{Role: role, Name: mem.Name, current: cur, staleID: stale[role]}
		if cur == nil {
			s.Action = ActionCreate
			steps = append(steps, s)
			continue
		}
		replace, upd, changes := diff(mem, cur)
		s.Changes = append(replace, changes...)
		s.update = upd
		switch {
		case len(replace) > 0:
			s.Action = ActionRecreate
			if cur.Kind == instance.KindVM && !opts.Recreate {
				blocked = append(blocked, role)
			}
		case len(changes) > 0:
			s.Action = ActionUpdate
		default:
			s.Action = ActionKeep
		}
		if s.Action != ActionRecreate && cur.State != instance.StateRunning {
			s.start = true
			s.Changes = append(s.Changes, fmt.Sprintf("start (currently %s)", cur.State))
			if s.Action == ActionKeep {
				s.Action = ActionStart
			}
		}
		steps = append(steps, s)
	}
	if len(blocked) > 0 {
		return steps, fmt.Errorf("apply: replacing VM %s would lose its disk; allow it with recreate", strings.Join(blocked, ", "))
	}
	return steps, nil
}

// diff compares a declared member with its instance. replace lists differences that need a
// new instance, changes the ones Update can apply in place.
func diff(mem Member, cur *instance.Spec) (replace []string, upd instance.UpdateParams, changes []string) {
	was := func(field string, from, to any) string { return fmt.Sprintf("%s %v -> %v", field, from, to) }
	if cur.Name != mem.Name {
		replace = append(replace, was("name", cur.Name, mem.Name))
	}
	if string(cur.Kind) != mem.Kind {
		return append(replace, was("kind", cur.Kind, mem.Kind)), upd, nil
	}

	switch {
	case mem.vm != nil && cur.VM != nil:
		want, have := mem.vm, cur.VM
		if want.ImageRef != have.ImageRef {
			replace = append(replace, was("image", have.ImageRef, want.ImageRef))
		}
		if want.CloudInitUserData != have.CloudInitUserData || want.CloudInitName != have.CloudInitName {
			replace = append(replace, "cloud-init changed")
		}
		if want.NoGuestAgent != have.NoGuestAgent {
			replace = append(replace, was("no_guest_agent", have.NoGuestAgent, want.NoGuestAgent))
		}
		for _, k := range want.SSHPublicKeys {
			if !slices.Contains(have.SSHPublicKeys, k) {
				replace = append(replace, "ssh_keys changed")
				break
			}
		}
		if want.CPUs != have.CPUs {
			upd.CPUs = &want.CPUs
			changes = append(changes, was("cpus", have.CPUs, want.CPUs))
		}
		if want.MemoryMiB != have.MemoryMiB {
			upd.MemoryMiB = &want.MemoryMiB
			changes = append(changes, was("memory", have.MemoryMiB, want.MemoryMiB))
		}
		// A disk of 0 means "catalog minimum", so it never asks for a change.
		switch {
		case want.DiskGiB == 0 || want.DiskGiB == have.DiskGiB:
		case have.DiskGiB != 0 && want.DiskGiB < have.DiskGiB:
			replace = append(replace, was("disk (can't shrink)", have.DiskGiB, want.DiskGiB))
		default:
			upd.DiskGiB = &want.DiskGiB
			changes = append(changes, was("disk", have.DiskGiB, want.DiskGiB))
		}
	case mem.container != nil && cur.Container != nil:
		want, have := mem.container, cur.Container
		haveEngine := have.Engine
		if haveEngine == "" {
			haveEngine = instance.ContainerEngineDocker
		}
		if want.ImageRef != have.ImageRef {
			replace = append(replace, was("image", have.ImageRef, want.ImageRef))
		}
		if want.Engine != haveEngine {
			replace = append(replace, was("engine", haveEngine, want.Engine))
		}
		if !maps.Equal(want.Env, have.Env) {
			replace = append(replace, "env changed")
		}
		if !slices.Equal(want.Entrypoint, have.Entrypoint) {
			replace = append(replace, was("entrypoint", have.Entrypoint, want.Entrypoint))
		}
		if !slices.Equal(want.Cmd, have.Cmd) {
			replace = append(replace, was("command", have.Cmd, want.Cmd))
		}
		if !slices.Equal(want.Volumes, have.Volumes) {
			replace = append(replace, "volumes changed")
		}
		if !slices.Equal(want.Ports, have.Ports) {
			replace = append(replace, was("ports", instance.FormatPorts(have.Ports), instance.FormatPorts(want.Ports)))
		}
	}

	if mem.Autostart != cur.Autostart {
		upd.Autostart = &mem.Autostart
		changes = append(changes, was("autostart", cur.Autostart, mem.Autostart))
	}
	if mem.restart.String() != cur.RestartPolicy.String() {
		upd.RestartPolicy = &mem.restart
		changes = append(changes, was("restart", cur.RestartPolicy.String(), mem.restart.String()))
	}
	return replace, upd, changes
}

func (a *Applier) run(ctx context.Context, m *Manifest, s Step, opts Options, progress func(string)) error {
	say := func(format string, args ...any) { progress(s.Role + ": " + fmt.Sprintf(format, args...)) }

	if s.Action == ActionDelete || s.Action == ActionRecreate {
		say("deleting %s", s.Name)
		if err := a.Instances.Delete(ctx, []string{s.current.Name}, true); err != nil {
			return err
		}
		s.staleID = s.current.ID
	}
	if s.staleID != "" {
		if _, err := a.Intents.Remove(ctx, m.Intent, s.staleID); err != nil {
			return err
		}
	}

	switch s.Action {
	case ActionDelete:
		return nil
	case ActionCreate, ActionRecreate:
		if err := a.launch(ctx, m, s.Role, opts, say); err != nil {
			return err
		}
	case ActionUpdate:
		res, err := a.Instances.Update(ctx, s.Name, s.update)
		if err != nil {
			return err
		}
		for _, note := range res.Notes {
			say("%s", note)
		}
	}
	if s.start {
		say("starting %s", s.Name)
		if err := a.Instances.Start(ctx, []string{s.Name}); err != nil {
			return err
		}
	}

	if _, declared := m.Members[s.Role]; declared && m.hasDependents(s.Role) {
		timeout := opts.WaitTimeout
		if timeout <= 0 {
			timeout = defaultWaitTimeout
		}
		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		say("waiting until ready")
		if err := a.Instances.WaitReady(wctx, s.Name, func(status string) { say("%s", status) }); err != nil {
			return fmt.Errorf("waiting for %s: %w", s.Name, err)
		}
	}
	return nil
}

func (a *Applier) launch(ctx context.Context, m *Manifest, role string, opts Options, say func(string, ...any)) error {
	mem := m.Members[role]
	params := instance.LaunchParams{
		Name:          mem.Name,
		Kind:          instance.Kind(mem.Kind),
		IntentName:    m.Intent,
		Role:          role,
		Autostart:     mem.Autostart,
		RestartPolicy: mem.restart,
	}
	// Copies, since the intent manager fills in network fields on them.
	if mem.vm != nil {
		v := *mem.vm
		v.SSHPublicKeys = append(slices.Clone(opts.SSHKeys), mem.vm.SSHPublicKeys...)
		params.VM = &v
	}
	if mem.container != nil {
		c := *mem.container
		params.Container = &c
	}
	say("creating %s", mem.Name)
	return a.Intents.Launch(ctx, params, func(ev instance.LaunchEvent) {
		if ev.Status != "" {
			say("%s", ev.Status)
		}
	})
}
