package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

const sample = `
intent: shop
members:
  db:
    kind: container
    image: postgres:16
    env: {POSTGRES_PASSWORD: secret}
    ports: ["5432:5432"]
  app:
    kind: vm
    image: ubuntu:24.04
    cpus: 2
    depends_on: [db, cache]
  cache:
    kind: container
    image: redis:7
`

func mustParse(t *testing.T, s string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(s), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func TestParseDefaultsAndOrder(t *testing.T) {
	m := mustParse(t, sample)
	app := m.Members["app"]
	if app.Name != "shop-app" || app.vm.MemoryMiB != defaultMemoryMiB || app.vm.CPUs != 2 {
		t.Fatalf("app = name %q memory %d cpus %d", app.Name, app.vm.MemoryMiB, app.vm.CPUs)
	}
	if db := m.Members["db"].container; db.Engine != instance.ContainerEngineDocker || db.Ports[0].Protocol != "tcp" {
		t.Fatalf("db container = %+v", db)
	}
	order, err := m.Order()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cache", "db", "app"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field": "intent: x\nmembers:\n  a: {kind: vm, image: u, cpu: 2}\n",
		"no members":    "intent: x\n",
		"bad kind":      "intent: x\nmembers:\n  a: {kind: pod, image: u}\n",
		"vm with env":   "intent: x\nmembers:\n  a: {kind: vm, image: u, env: {A: b}}\n",
		"vm with ports": "intent: x\nmembers:\n  a: {kind: vm, image: u, ports: ['80:80']}\n",
		"ct with cpus":  "intent: x\nmembers:\n  a: {kind: container, image: u, cpus: 2}\n",
		"relative vol":  "intent: x\nmembers:\n  a: {kind: container, image: u, volumes: ['data:/d']}\n",
		"unknown dep":   "intent: x\nmembers:\n  a: {kind: container, image: u, depends_on: [b]}\n",
		"cycle": "intent: x\nmembers:\n  a: {kind: container, image: u, depends_on: [b]}\n" +
			"  b: {kind: container, image: u, depends_on: [a]}\n",
		"bad hypervisor": "intent: x\nmembers:\n  a: {kind: vm, image: u, hypervisor: xen}\n",
		"ct hypervisor":  "intent: x\nmembers:\n  a: {kind: container, image: u, hypervisor: qemu}\n",
		"ssh key path":   "intent: x\nmembers:\n  a: {kind: vm, image: u, ssh_keys: [~/.ssh/id.pub]}\n",
		"bad device":     "intent: x\nmembers:\n  a: {kind: vm, image: u, devices: [sda]}\n",
		"ct device":      "intent: x\nmembers:\n  a: {kind: container, image: u, devices: ['usb:3-2']}\n",
	}
	for name, s := range cases {
		if _, err := Parse([]byte(s), nil); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}

// fakes records every call in one log, so tests can check the order of operations.
type fakes struct {
	mu      sync.Mutex
	log     []string
	intent  *store.Intent
	specs   map[string]*instance.Spec // by ID
	updated instance.UpdateParams

	held, reserved []string
	holdErr        error
}

func (f *fakes) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, call)
}

// inLevels checks got ran levels one after another: every call of a level before any of
// the next, each step's own calls in order, while the steps of one level may interleave.
func inLevels(t *testing.T, got []string, levels ...[][]string) {
	t.Helper()
	i := 0
	for n, level := range levels {
		next := make([]int, len(level)) // per step: how many of its calls were seen
		left := 0
		for _, step := range level {
			left += len(step)
		}
		for ; left > 0; left-- {
			if i >= len(got) {
				t.Fatalf("calls = %v: level %d missing calls of %v", got, n, level)
			}
			found := false
			for j, step := range level {
				if next[j] < len(step) && step[next[j]] == got[i] {
					next[j]++
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("calls = %v: %q (call %d) is not next in level %d %v", got, got[i], i, n, level)
			}
			i++
		}
	}
	if i != len(got) {
		t.Fatalf("calls = %v: unexpected calls after %d", got, i)
	}
}

func (f *fakes) Hold(ctx context.Context, names []string) (context.Context, func(), error) {
	if f.holdErr != nil {
		return nil, nil, f.holdErr
	}
	f.held = append(f.held, names...)
	return ctx, func() {}, nil
}

func (f *fakes) ReserveNames(ctx context.Context, names []string) (context.Context, func(), error) {
	f.reserved = append(f.reserved, names...)
	return ctx, func() {}, nil
}

func (f *fakes) Info(name string) (store.Intent, error) {
	if f.intent == nil {
		return store.Intent{}, fmt.Errorf("no intent: %w", instance.ErrNotFound)
	}
	return *f.intent, nil
}

func (f *fakes) Launch(ctx context.Context, p instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	f.record("launch " + p.Role)
	if p.VM != nil && !slices.Contains(p.VM.SSHPublicKeys, "ssh-ed25519 CALLER") {
		return fmt.Errorf("caller key missing from %v", p.VM.SSHPublicKeys)
	}
	progress(instance.LaunchEvent{Status: "booting"})
	return nil
}

func (f *fakes) Remove(ctx context.Context, name, member string) (store.Intent, error) {
	f.record("remove " + member)
	return store.Intent{}, nil
}

func (f *fakes) GetByID(id string) (*instance.Spec, error) {
	if s, ok := f.specs[id]; ok {
		return s, nil
	}
	return nil, instance.ErrNotFound
}

func (f *fakes) Start(ctx context.Context, names []string) error {
	f.record("start " + strings.Join(names, ","))
	return nil
}

func (f *fakes) Delete(ctx context.Context, names []string) error {
	f.record("delete " + strings.Join(names, ","))
	return nil
}

func (f *fakes) Update(ctx context.Context, name string, p instance.UpdateParams) (instance.UpdateResult, error) {
	f.record("update " + name)
	f.mu.Lock()
	f.updated = p
	f.mu.Unlock()
	return instance.UpdateResult{}, nil
}

func (f *fakes) WaitReady(ctx context.Context, name string, progress func(string)) error {
	f.record("wait " + name)
	return nil
}

// member adds an existing member built from what the manifest declares for role.
func (f *fakes) member(m *Manifest, role string, state instance.State) *instance.Spec {
	mem := m.Members[role]
	spec := &instance.Spec{ID: "id-" + role, Name: mem.Name, Kind: instance.Kind(mem.Kind), State: state}
	if mem.vm != nil {
		v := *mem.vm
		v.SSHPublicKeys = append([]string{"ssh-ed25519 CALLER"}, v.SSHPublicKeys...)
		spec.VM = &v
	}
	if mem.container != nil {
		c := *mem.container
		spec.Container = &c
	}
	return f.add(role, spec)
}

func (f *fakes) add(role string, spec *instance.Spec) *instance.Spec {
	if f.intent == nil {
		f.intent = &store.Intent{ID: "it", Name: "shop"}
	}
	if f.specs == nil {
		f.specs = map[string]*instance.Spec{}
	}
	f.specs[spec.ID] = spec
	f.intent.Members = append(f.intent.Members, store.IntentMember{InstanceID: spec.ID, Role: role, Kind: spec.Kind})
	return spec
}

func runApply(t *testing.T, f *fakes, m *Manifest, opts Options) (map[string]Step, error) {
	t.Helper()
	opts.SSHKeys = []string{"ssh-ed25519 CALLER"}
	a := &Applier{Instances: f, Intents: f}
	steps := map[string]Step{}
	err := a.Apply(context.Background(), m, opts, func(ss []Step) {
		for _, s := range ss {
			steps[s.Role] = s
		}
	}, func(string) {})
	return steps, err
}

func TestApplyCreatesInDependencyOrder(t *testing.T) {
	f := &fakes{}
	steps, err := runApply(t, f, mustParse(t, sample), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for role, s := range steps {
		if s.Action != ActionCreate {
			t.Errorf("%s: action %s, want create", role, s.Action)
		}
	}
	inLevels(t, f.log,
		[][]string{{"launch cache", "wait shop-cache"}, {"launch db", "wait shop-db"}},
		[][]string{{"launch app"}})
}

func TestApplyNoChanges(t *testing.T) {
	m := mustParse(t, sample)
	f := &fakes{}
	for _, role := range []string{"db", "cache", "app"} {
		f.member(m, role, instance.StateRunning)
	}
	steps, err := runApply(t, f, m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for role, s := range steps {
		if s.Action != ActionKeep {
			t.Errorf("%s: action %s (%v), want keep", role, s.Action, s.Changes)
		}
	}
	// Dependencies are still checked for readiness, nothing else is touched.
	inLevels(t, f.log, [][]string{{"wait shop-cache"}, {"wait shop-db"}})
}

func TestApplyDiff(t *testing.T) {
	m := mustParse(t, sample)
	f := &fakes{}
	db := f.member(m, "db", instance.StateRunning)
	db.Container.Env = map[string]string{"POSTGRES_PASSWORD": "old"}
	app := f.member(m, "app", instance.StateStopped)
	app.VM.CPUs = 1
	f.member(m, "cache", instance.StateRunning)
	f.add("old", &instance.Spec{ID: "id-old", Name: "shop-old", Kind: instance.KindContainer, Container: &instance.ContainerSpec{}})

	steps, err := runApply(t, f, m, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if steps["db"].Action != ActionRecreate || steps["app"].Action != ActionUpdate || steps["old"].Action != ActionKeep {
		t.Fatalf("actions db=%s app=%s old=%s", steps["db"].Action, steps["app"].Action, steps["old"].Action)
	}
	if len(f.log) != 0 {
		t.Fatalf("dry run made calls: %v", f.log)
	}

	if _, err := runApply(t, f, m, Options{Prune: true}); err != nil {
		t.Fatal(err)
	}
	inLevels(t, f.log,
		[][]string{{"delete shop-old", "remove id-old"}},
		[][]string{{"wait shop-cache"}, {"delete shop-db", "remove id-db", "launch db", "wait shop-db"}},
		[][]string{{"update shop-app", "start shop-app"}})
	if f.updated.CPUs == nil || *f.updated.CPUs != 2 || f.updated.MemoryMiB != nil {
		t.Fatalf("update params = %+v, want only cpus=2", f.updated)
	}
}

func TestApplyVMReplaceNeedsRecreate(t *testing.T) {
	m := mustParse(t, sample)
	f := &fakes{}
	f.member(m, "app", instance.StateRunning).VM.ImageRef = "ubuntu:22.04"

	steps, err := runApply(t, f, m, Options{})
	if err == nil || steps["app"].Action != ActionRecreate {
		t.Fatalf("err = %v, app action = %s; want a recreate refused", err, steps["app"].Action)
	}
	if len(f.log) != 0 {
		t.Fatalf("refused plan made calls: %v", f.log)
	}
	if _, err := runApply(t, f, m, Options{Recreate: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "delete shop-app") || !slices.Contains(f.log, "launch app") {
		t.Fatalf("calls = %v, want app deleted and launched again", f.log)
	}
}

// docs/example.yaml documents every field, so it must keep parsing.
func TestDocsExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../../docs/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(data, nil)
	if err != nil {
		t.Fatalf("docs/example.yaml: %v", err)
	}
	if len(m.Members) != 4 {
		t.Fatalf("got %d members, want 4", len(m.Members))
	}
}

func TestApplyHypervisorChange(t *testing.T) {
	m := mustParse(t, sample)
	f := &fakes{}
	// A VM stored before hypervisors existed has none set, which means qemu: no change.
	f.member(m, "app", instance.StateRunning).VM.Hypervisor = ""
	steps, err := runApply(t, f, m, Options{DryRun: true})
	if err != nil || steps["app"].Action != ActionKeep {
		t.Fatalf("err = %v, app action = %s (%v); want keep", err, steps["app"].Action, steps["app"].Changes)
	}

	f.specs["id-app"].VM.Hypervisor = instance.VMHypervisorFirecracker
	steps, _ = runApply(t, f, m, Options{DryRun: true})
	if steps["app"].Action != ActionRecreate {
		t.Fatalf("app action = %s, want recreate after a hypervisor change", steps["app"].Action)
	}
}

func TestApplyLocksMembersAndNames(t *testing.T) {
	m := mustParse(t, sample)
	f := &fakes{}
	f.member(m, "cache", instance.StateRunning)
	if _, err := runApply(t, f, m, Options{}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(f.reserved)
	if !slices.Equal(f.held, []string{"shop-cache"}) || !slices.Equal(f.reserved, []string{"shop-app", "shop-db"}) {
		t.Errorf("held %v, reserved %v; want [shop-cache], [shop-app shop-db]", f.held, f.reserved)
	}

	busy := &fakes{holdErr: instance.ErrBusy}
	busy.member(m, "cache", instance.StateRunning)
	if _, err := runApply(t, busy, m, Options{}); !errors.Is(err, instance.ErrBusy) {
		t.Errorf("expected a busy member to stop the apply, got %v", err)
	}
	if len(busy.log) != 0 {
		t.Errorf("expected nothing run, got %v", busy.log)
	}
}

func TestSamePortReadsEmptyProtocolAsTCP(t *testing.T) {
	a := instance.PortMapping{HostPort: 8080, GuestPort: 80}
	b := instance.PortMapping{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}
	if !samePort(a, b) {
		t.Error(`expected "" and "tcp" to be the same port`)
	}
	if samePort(a, instance.PortMapping{HostPort: 8080, GuestPort: 80, Protocol: "udp"}) {
		t.Error("expected tcp and udp to differ")
	}
}

func TestDevicesNeedReplace(t *testing.T) {
	m, err := Parse([]byte("intent: x\nmembers:\n  a: {kind: vm, image: u, devices: ['USB:3-2']}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := m.Members["a"]
	if !slices.Equal(mem.vm.Devices, []string{"usb:3-2"}) {
		t.Fatalf("devices = %v", mem.vm.Devices)
	}
	cur := &instance.Spec{Name: mem.Name, Kind: instance.KindVM, VM: &instance.VMSpec{
		ImageRef: "u", CPUs: mem.vm.CPUs, MemoryMiB: mem.vm.MemoryMiB, Hypervisor: instance.VMHypervisorQEMU,
	}}
	replace, _, _ := diff(mem, cur)
	if len(replace) != 1 || !strings.Contains(replace[0], "devices") {
		t.Errorf("replace = %v, want a devices change", replace)
	}
}

func TestParseVars(t *testing.T) {
	s := "vars: {IMG: u, CPUS: 1}\nintent: ${NAME}\nmembers:\n  a:\n    kind: vm\n    image: $IMG\n    cpus: ${CPUS}\n"
	m, err := Parse([]byte(s), map[string]string{"NAME": "x", "CPUS": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if a := m.Members["a"]; m.Intent != "x" || a.Image != "u" || a.vm.CPUs != 3 {
		t.Errorf("got intent %q, member %+v", m.Intent, a)
	}
	if _, err := Parse([]byte(s), nil); err == nil || !strings.Contains(err.Error(), "NAME is not set") {
		t.Errorf("err = %v, want NAME is not set", err)
	}
}

func TestStarterParses(t *testing.T) {
	m := mustParse(t, Starter("demo"))
	if m.Intent != "demo" || len(m.Members) != 1 {
		t.Fatalf("Starter parsed to %+v", m)
	}
}

func TestCheckLines(t *testing.T) {
	cases := map[string]int{
		"intent: x\nmembers:\n  a:\n    kind: vm\n    image: u\n    cpu: 2\n":                      6, // unknown field
		"intent: x\nmembers:\n  a:\n    kind: pod\n    image: u\n":                                 3, // the member's role
		"intent: x\nmembers:\n  a:\n    kind: vm\n    image: ${IMG}\n    cpu: 2\n":                 5, // unset var comes first
		"intent: x\nvars: {IMG: u}\nmembers:\n  a:\n    kind: vm\n    image: ${IMG}\n    cpu: 2\n": 7, // re-encoded YAML, found by name
		"intent: x\nmembers:\n  a: [\n":                                                            3,
	}
	for data, want := range cases {
		ps := Check([]byte(data), nil)
		if len(ps) != 1 || ps[0].Line != want {
			t.Errorf("%q: got %+v, want line %d", data, ps, want)
		}
	}
	if ps := Check([]byte(Starter("demo")), nil); ps != nil {
		t.Errorf("Starter: %+v", ps)
	}
}

// blockingFakes holds every launch until all of a level's launches are in, failing one role.
type blockingFakes struct {
	*fakes
	entered chan string
	release chan struct{}
	fail    string
}

func (f *blockingFakes) Launch(ctx context.Context, p instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	f.entered <- p.Role
	<-f.release
	if p.Role == f.fail {
		return errors.New("boot failed")
	}
	return f.fakes.Launch(ctx, p, progress)
}

func TestApplyLevelRunsInParallel(t *testing.T) {
	f := &blockingFakes{fakes: &fakes{}, entered: make(chan string, 3), release: make(chan struct{}), fail: "db"}
	a := &Applier{Instances: f, Intents: f}
	done := make(chan error, 1)
	go func() {
		done <- a.Apply(context.Background(), mustParse(t, sample), Options{SSHKeys: []string{"ssh-ed25519 CALLER"}}, func([]Step) {}, func(string) {})
	}()
	got := map[string]bool{}
	for range 2 { // cache and db, which app depends on, launch together
		select {
		case role := <-f.entered:
			got[role] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v launched: the level ran one at a time", got)
		}
	}
	if !got["cache"] || !got["db"] {
		t.Fatalf("first level launched %v", got)
	}
	close(f.release)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "db (create)") {
		t.Fatalf("Apply = %v, want db's failure", err)
	}
	// A failed level stops the apply: app, which depends on db, never launches.
	if len(f.entered) != 0 || slices.Contains(f.log, "launch app") {
		t.Errorf("app launched after its dependency failed: %v", f.log)
	}
}
