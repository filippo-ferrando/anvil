// Package apply compares an intent manifest (anvil.yaml) with the current state and applies the difference.
package apply

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/anvil-project/anvil/internal/instance"
)

// Manifest is one parsed anvil.yaml: an intent and its members, keyed by role.
type Manifest struct {
	Intent  string            `yaml:"intent"`
	Members map[string]Member `yaml:"members"`
}

// Member is one declared instance. VM-only and container-only fields are rejected on the other kind.
type Member struct {
	Kind      string   `yaml:"kind"`
	Image     string   `yaml:"image"`
	Name      string   `yaml:"name"`
	DependsOn []string `yaml:"depends_on"`
	Autostart bool     `yaml:"autostart"`
	Restart   string   `yaml:"restart"`

	CPUs          int      `yaml:"cpus"`
	Memory        int64    `yaml:"memory"` // MiB
	Disk          int64    `yaml:"disk"`   // GiB, 0 = catalog minimum
	CloudInit     string   `yaml:"cloud_init"`
	CloudInitName string   `yaml:"cloud_init_name"`
	SSHKeys       []string `yaml:"ssh_keys"`
	NoGuestAgent  bool     `yaml:"no_guest_agent"`

	Engine     string            `yaml:"engine"`
	Env        map[string]string `yaml:"env"`
	Entrypoint []string          `yaml:"entrypoint"`
	Command    []string          `yaml:"command"`
	Volumes    []string          `yaml:"volumes"`
	Ports      []string          `yaml:"ports"`

	// Filled by Parse from the fields above.
	restart   instance.RestartPolicy
	vm        *instance.VMSpec
	container *instance.ContainerSpec
}

// Defaults match `anvil launch`.
const (
	defaultCPUs      = 1
	defaultMemoryMiB = 1024
)

// Parse reads and validates a manifest. Unknown fields are errors, so typos don't pass silently.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("apply: reading manifest: %w", err)
	}
	if m.Intent == "" {
		return nil, errors.New("apply: manifest needs an intent name")
	}
	if len(m.Members) == 0 {
		return nil, fmt.Errorf("apply: intent %q declares no members", m.Intent)
	}
	names := map[string]string{}
	for role, mem := range m.Members {
		if err := mem.parse(m.Intent, role); err != nil {
			return nil, fmt.Errorf("apply: member %q: %w", role, err)
		}
		if other, dup := names[mem.Name]; dup {
			return nil, fmt.Errorf("apply: members %q and %q both use the name %q", other, role, mem.Name)
		}
		names[mem.Name] = role
		for _, dep := range mem.DependsOn {
			if _, ok := m.Members[dep]; !ok || dep == role {
				return nil, fmt.Errorf("apply: member %q depends on unknown member %q", role, dep)
			}
		}
		m.Members[role] = mem
	}
	if _, err := m.Order(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (mem *Member) parse(intentName, role string) error {
	if role == "" {
		return errors.New("role can't be empty")
	}
	if mem.Image == "" {
		return errors.New("image is required")
	}
	if mem.Name == "" {
		mem.Name = intentName + "-" + role
	}
	policy, err := instance.ParseRestartPolicy(mem.Restart)
	if err != nil {
		return err
	}
	mem.restart = policy

	switch mem.Kind {
	case "vm":
		return mem.parseVM()
	case "container":
		return mem.parseContainer()
	default:
		return fmt.Errorf(`kind must be "vm" or "container", got %q`, mem.Kind)
	}
}

func (mem *Member) parseVM() error {
	if mem.Engine != "" || len(mem.Env) > 0 || len(mem.Entrypoint) > 0 || len(mem.Command) > 0 || len(mem.Volumes) > 0 {
		return errors.New("engine, env, entrypoint, command and volumes only apply to a container")
	}
	if len(mem.Ports) > 0 {
		return errors.New("ports don't apply to a VM in an intent: it gets its own address on the intent network")
	}
	if mem.CloudInit != "" && mem.CloudInitName != "" {
		return errors.New("cloud_init and cloud_init_name are mutually exclusive")
	}
	for _, k := range mem.SSHKeys {
		if !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") && !strings.HasPrefix(k, "sk-") {
			return fmt.Errorf("ssh_keys must be literal public keys, got %q", k)
		}
	}
	if mem.CPUs < 0 || mem.Memory < 0 || mem.Disk < 0 {
		return errors.New("cpus, memory and disk can't be negative")
	}
	v := &instance.VMSpec{
		ImageRef:          mem.Image,
		CPUs:              mem.CPUs,
		MemoryMiB:         mem.Memory,
		DiskGiB:           mem.Disk,
		CloudInitUserData: mem.CloudInit,
		CloudInitName:     mem.CloudInitName,
		SSHPublicKeys:     mem.SSHKeys,
		NoGuestAgent:      mem.NoGuestAgent,
	}
	if v.CPUs == 0 {
		v.CPUs = defaultCPUs
	}
	if v.MemoryMiB == 0 {
		v.MemoryMiB = defaultMemoryMiB
	}
	mem.vm = v
	return nil
}

func (mem *Member) parseContainer() error {
	if mem.CPUs != 0 || mem.Memory != 0 || mem.Disk != 0 || mem.CloudInit != "" || mem.CloudInitName != "" ||
		len(mem.SSHKeys) > 0 || mem.NoGuestAgent {
		return errors.New("cpus, memory, disk, cloud_init, cloud_init_name, ssh_keys and no_guest_agent only apply to a VM")
	}
	c := &instance.ContainerSpec{
		ImageRef:   mem.Image,
		Env:        mem.Env,
		Entrypoint: mem.Entrypoint,
		Cmd:        mem.Command,
	}
	switch mem.Engine {
	case "", "docker":
		c.Engine = instance.ContainerEngineDocker
	case "podman":
		c.Engine = instance.ContainerEnginePodman
	default:
		return fmt.Errorf(`engine must be "docker" or "podman", got %q`, mem.Engine)
	}
	for _, s := range mem.Volumes {
		v, err := instance.ParseVolumeMount(s)
		if err != nil {
			return err
		}
		// The daemon has no working directory of the caller to resolve against.
		if !filepath.IsAbs(v.HostPath) {
			return fmt.Errorf("volume %q: host path must be absolute", s)
		}
		c.Volumes = append(c.Volumes, v)
	}
	for _, s := range mem.Ports {
		p, err := instance.ParsePortMapping(s)
		if err != nil {
			return err
		}
		c.Ports = append(c.Ports, p)
	}
	mem.container = c
	return nil
}

// Order returns the roles so every member comes after the members it depends on.
// Ties are broken by name, so the order is the same on every run.
func (m *Manifest) Order() ([]string, error) {
	pending := map[string]int{}
	dependents := map[string][]string{}
	for role, mem := range m.Members {
		pending[role] = len(mem.DependsOn)
		for _, dep := range mem.DependsOn {
			dependents[dep] = append(dependents[dep], role)
		}
	}
	var ready, order []string
	for role, n := range pending {
		if n == 0 {
			ready = append(ready, role)
		}
	}
	for len(ready) > 0 {
		sort.Strings(ready)
		role := ready[0]
		ready = ready[1:]
		order = append(order, role)
		for _, d := range dependents[role] {
			if pending[d]--; pending[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(order) != len(m.Members) {
		var stuck []string
		for role, n := range pending {
			if n > 0 {
				stuck = append(stuck, role)
			}
		}
		slices.Sort(stuck)
		return nil, fmt.Errorf("apply: depends_on has a cycle between %s", strings.Join(stuck, ", "))
	}
	return order, nil
}

// hasDependents reports whether any member lists role in depends_on.
func (m *Manifest) hasDependents(role string) bool {
	for _, mem := range m.Members {
		if slices.Contains(mem.DependsOn, role) {
			return true
		}
	}
	return false
}
