// Package pipeline runs anvil-ci.yaml files: jobs on ephemeral clones, ordered by their
// needs, by hand or on every new commit of a watched branch.
package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFile is the pipeline file a run reads when none is named.
const DefaultFile = "anvil-ci.yaml"

// DefaultTimeout bounds a job that sets no timeout.
const DefaultTimeout = time.Hour

// Pipeline is a parsed pipeline file.
type Pipeline struct {
	Jobs  map[string]*Job `yaml:"jobs"`
	Apply string          `yaml:"apply"` // an anvil.yaml applied once every job passed
	order []string
}

// Job runs one command on one clone per distro.
type Job struct {
	Distros  []string          `yaml:"distros"`
	Distro   string            `yaml:"distro"` // shorthand for one distro
	Run      string            `yaml:"run"`    // run with sh -c from /work
	Needs    []string          `yaml:"needs"`
	Env      map[string]string `yaml:"env"`
	Setup    string            `yaml:"setup"` // a setup script in the repo, see anvil warm create --setup
	KeyFiles []string          `yaml:"key-files"`
	Collect  []string          `yaml:"collect"` // guest paths or globs kept with the run
	Timeout  string            `yaml:"timeout"`
	CPUs     int               `yaml:"cpus"`
	Memory   int64             `yaml:"memory"` // MiB
	timeout  time.Duration
}

// Parse reads and checks a pipeline file.
func Parse(data []byte) (*Pipeline, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Pipeline
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	if len(p.Jobs) == 0 {
		return nil, errors.New("pipeline: no jobs")
	}
	for name, j := range p.Jobs {
		if j == nil {
			return nil, fmt.Errorf("pipeline: job %s is empty", name)
		}
		if err := j.check(name); err != nil {
			return nil, err
		}
		for _, n := range j.Needs {
			if _, ok := p.Jobs[n]; !ok {
				return nil, fmt.Errorf("pipeline: job %s needs %s, which isn't a job", name, n)
			}
		}
	}
	if p.Apply != "" {
		if err := relPath(p.Apply); err != nil {
			return nil, fmt.Errorf("pipeline: apply: %w", err)
		}
	}
	order, err := topoOrder(p.Jobs)
	if err != nil {
		return nil, err
	}
	p.order = order
	return &p, nil
}

func (j *Job) check(name string) error {
	if name == "" || strings.ContainsAny(name, "/@ ") {
		return fmt.Errorf("pipeline: job name %q: no '/', '@' or spaces", name)
	}
	if j.Distro != "" {
		if len(j.Distros) > 0 {
			return fmt.Errorf("pipeline: job %s: set distro or distros, not both", name)
		}
		j.Distros = []string{j.Distro}
	}
	if len(j.Distros) == 0 {
		return fmt.Errorf("pipeline: job %s: no distro", name)
	}
	seen := map[string]bool{}
	for _, d := range j.Distros {
		if d == "" || strings.ContainsAny(d, "/ ") || seen[d] {
			return fmt.Errorf("pipeline: job %s: bad or repeated distro %q", name, d)
		}
		seen[d] = true
	}
	if strings.TrimSpace(j.Run) == "" {
		return fmt.Errorf("pipeline: job %s: nothing to run", name)
	}
	if j.Setup != "" {
		if err := relPath(j.Setup); err != nil {
			return fmt.Errorf("pipeline: job %s: setup: %w", name, err)
		}
	} else if len(j.KeyFiles) > 0 {
		return fmt.Errorf("pipeline: job %s: key-files needs setup", name)
	}
	for _, f := range j.KeyFiles {
		if err := relPath(f); err != nil {
			return fmt.Errorf("pipeline: job %s: key file: %w", name, err)
		}
	}
	for k := range j.Env {
		if k == "" || strings.ContainsAny(k, "= ") {
			return fmt.Errorf("pipeline: job %s: bad env name %q", name, k)
		}
	}
	j.timeout = DefaultTimeout
	if j.Timeout != "" {
		d, err := time.ParseDuration(j.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("pipeline: job %s: timeout %q, want a duration like 20m", name, j.Timeout)
		}
		j.timeout = d
	}
	if j.CPUs < 0 || j.Memory < 0 {
		return fmt.Errorf("pipeline: job %s: cpus and memory can't be negative", name)
	}
	return nil
}

// relPath accepts a relative path that stays inside the source.
func relPath(p string) error {
	c := path.Clean(p)
	if path.IsAbs(p) || c == ".." || strings.HasPrefix(c, "../") || c == "." {
		return fmt.Errorf("%q must be a path inside the repo", p)
	}
	return nil
}

// topoOrder orders jobs so each comes after its needs, by name among equals.
func topoOrder(jobs map[string]*Job) ([]string, error) {
	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	state := map[string]int{} // 1 visiting, 2 done
	var order []string
	var visit func(n string, stack []string) error
	visit = func(n string, stack []string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("pipeline: jobs need each other: %s", strings.Join(append(stack, n), " -> "))
		case 2:
			return nil
		}
		state[n] = 1
		needs := append([]string(nil), jobs[n].Needs...)
		sort.Strings(needs)
		for _, d := range needs {
			if err := visit(d, append(stack, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		order = append(order, n)
		return nil
	}
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Order is the jobs in the order they are listed in a run.
func (p *Pipeline) Order() []string { return p.order }
