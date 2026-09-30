package apply

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
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
	m, err := Parse([]byte(s))
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
		"ssh key path": "intent: x\nmembers:\n  a: {kind: vm, image: u, ssh_keys: [~/.ssh/id.pub]}\n",
	}
	for name, s := range cases {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}

// fakes records every call in one log, so tests can check the order of operations.
type fakes struct {
	log     []string
	intent  *store.Intent
	specs   map[string]*instance.Spec // by ID
	updated instance.UpdateParams
}

func (f *fakes) Info(name string) (store.Intent, error) {
	if f.intent == nil {
		return store.Intent{}, fmt.Errorf("no intent: %w", instance.ErrNotFound)
	}
	return *f.intent, nil
}

func (f *fakes) Launch(ctx context.Context, p instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	f.log = append(f.log, "launch "+p.Role)
	if p.VM != nil && !slices.Contains(p.VM.SSHPublicKeys, "ssh-ed25519 CALLER") {
		return fmt.Errorf("caller key missing from %v", p.VM.SSHPublicKeys)
	}
	progress(instance.LaunchEvent{Status: "booting"})
	return nil
}

func (f *fakes) Remove(ctx context.Context, name, member string) (store.Intent, error) {
	f.log = append(f.log, "remove "+member)
	return store.Intent{}, nil
}

func (f *fakes) GetByID(id string) (*instance.Spec, error) {
	if s, ok := f.specs[id]; ok {
		return s, nil
	}
	return nil, instance.ErrNotFound
}

func (f *fakes) Start(ctx context.Context, names []string) error {
	f.log = append(f.log, "start "+strings.Join(names, ","))
	return nil
}

func (f *fakes) Delete(ctx context.Context, names []string, purge bool) error {
	f.log = append(f.log, "delete "+strings.Join(names, ","))
	return nil
}

func (f *fakes) Update(ctx context.Context, name string, p instance.UpdateParams) (instance.UpdateResult, error) {
	f.log = append(f.log, "update "+name)
	f.updated = p
	return instance.UpdateResult{}, nil
}

func (f *fakes) WaitReady(ctx context.Context, name string, progress func(string)) error {
	f.log = append(f.log, "wait "+name)
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
	want := []string{"launch cache", "wait shop-cache", "launch db", "wait shop-db", "launch app"}
	if !slices.Equal(f.log, want) {
		t.Fatalf("calls = %v\nwant    %v", f.log, want)
	}
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
	if want := []string{"wait shop-cache", "wait shop-db"}; !slices.Equal(f.log, want) {
		t.Fatalf("calls = %v, want %v", f.log, want)
	}
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
	want := []string{
		"delete shop-old", "remove id-old",
		"wait shop-cache",
		"delete shop-db", "remove id-db", "launch db", "wait shop-db",
		"update shop-app", "start shop-app",
	}
	if !slices.Equal(f.log, want) {
		t.Fatalf("calls = %v\nwant    %v", f.log, want)
	}
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
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("docs/example.yaml: %v", err)
	}
	if len(m.Members) != 4 {
		t.Fatalf("got %d members, want 4", len(m.Members))
	}
}
