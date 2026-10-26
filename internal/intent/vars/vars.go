// Package vars expands compose-style variables (${VAR}, ${VAR:-default}, ...) in an anvil.yaml manifest.
package vars

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Interpolate expands variables in every scalar value of a manifest. Keys and the top-level
// vars: block are left alone. given wins over the manifest's own vars: block.
func Interpolate(data []byte, given map[string]string) ([]byte, error) {
	if !bytes.Contains(data, []byte("$")) {
		return data, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	if doc.Kind == 0 {
		return data, nil
	}
	defaults := manifestVars(&doc)
	lookup := func(name string) (string, bool) {
		if v, ok := given[name]; ok {
			return v, true
		}
		v, ok := defaults[name]
		return v, ok
	}
	var problems []string
	var syntaxErr error
	walk(&doc, func(n *yaml.Node) {
		v, p, err := expand(n.Value, lookup)
		if err != nil {
			syntaxErr = errors.Join(syntaxErr, err)
			return
		}
		problems = append(problems, p...)
		if v != n.Value {
			n.Value = v
			if n.Style == 0 {
				n.Tag = "" // let a plain value resolve again, so "cpus: ${CPUS}" is still a number
			}
		}
	})
	if syntaxErr != nil {
		return nil, syntaxErr
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return nil, fmt.Errorf("manifest variables: %s", strings.Join(slices.Compact(problems), "; "))
	}
	return yaml.Marshal(&doc)
}

// Names lists the variables a manifest's values reference, sorted.
func Names(data []byte) ([]string, error) {
	if !bytes.Contains(data, []byte("$")) {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	var names []string
	record := func(name string) (string, bool) {
		names = append(names, name)
		return "", false
	}
	walk(&doc, func(n *yaml.Node) { expand(n.Value, record) })
	slices.Sort(names)
	return slices.Compact(names), nil
}

// Load builds the vars a client sends with a manifest: overrides, then the shell env, then the
// .env file, for the names the manifest uses. envFile "" means .env beside the manifest, if any.
func Load(manifestPath, envFile string, data []byte, overrides map[string]string) (map[string]string, error) {
	path := envFile
	if path == "" {
		dir := "."
		if manifestPath != "-" {
			dir = filepath.Dir(manifestPath)
		}
		path = filepath.Join(dir, ".env")
	}
	dotenv, err := ReadEnvFile(path)
	if err != nil && (envFile != "" || !errors.Is(err, fs.ErrNotExist)) {
		return nil, err
	}
	names, err := Names(data)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range overrides {
		out[k] = v
	}
	for _, name := range names {
		if _, ok := out[name]; ok {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			out[name] = v
		} else if v, ok := dotenv[name]; ok {
			out[name] = v
		}
	}
	return out, nil
}

// ReadEnvFile reads KEY=VALUE lines. Blank lines, "#" comments and an "export " prefix are
// allowed, and a value can be wrapped in matching quotes.
func ReadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading env file: %w", err)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !validName(key) {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out, sc.Err()
}

// manifestVars reads the top-level vars: map, the manifest's own defaults.
func manifestVars(doc *yaml.Node) map[string]string {
	out := map[string]string{}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "vars" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		m := root.Content[i+1].Content
		for j := 0; j+1 < len(m); j += 2 {
			out[m[j].Value] = m[j+1].Value
		}
	}
	return out
}

// walk calls fn on every scalar value, skipping mapping keys and the top-level vars: block.
func walk(doc *yaml.Node, fn func(*yaml.Node)) {
	var visit func(n *yaml.Node, top bool)
	visit = func(n *yaml.Node, top bool) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				visit(c, true)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				if top && n.Content[i].Value == "vars" {
					continue
				}
				visit(n.Content[i+1], false)
			}
		case yaml.SequenceNode:
			for _, c := range n.Content {
				visit(c, false)
			}
		case yaml.ScalarNode:
			fn(n)
		}
	}
	visit(doc, false)
}

// expand replaces variables in s. Unset variables and ${VAR:?msg} failures come back as
// problems, so every one can be reported at once; err is only for broken syntax.
func expand(s string, lookup func(string) (string, bool)) (out string, problems []string, err error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '$':
			b.WriteByte('$')
			i++
		case i+1 < len(s) && s[i+1] == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", nil, fmt.Errorf("unclosed ${ in %q", s)
			}
			v, p, err := expandExpr(s[i+2:i+2+end], lookup)
			if err != nil {
				return "", nil, err
			}
			b.WriteString(v)
			problems = append(problems, p...)
			i += 2 + end
		default:
			j := i + 1
			for j < len(s) && isNameChar(s[j], j == i+1) {
				j++
			}
			if j == i+1 {
				b.WriteByte('$') // a lone "$" stays as it is
				continue
			}
			name := s[i+1 : j]
			if v, ok := lookup(name); ok {
				b.WriteString(v)
			} else {
				problems = append(problems, fmt.Sprintf("%s is not set", name))
			}
			i = j - 1
		}
	}
	return b.String(), problems, nil
}

// expandExpr handles the inside of ${...}: NAME, NAME:-def, NAME-def, NAME:?msg or NAME?msg.
func expandExpr(expr string, lookup func(string) (string, bool)) (string, []string, error) {
	n := 0
	for n < len(expr) && isNameChar(expr[n], n == 0) {
		n++
	}
	name, rest := expr[:n], expr[n:]
	if name == "" {
		return "", nil, fmt.Errorf("bad variable ${%s}", expr)
	}
	v, ok := lookup(name)
	switch {
	case rest == "":
		if !ok {
			return "", []string{name + " is not set"}, nil
		}
	case strings.HasPrefix(rest, ":-"):
		if v == "" {
			v = rest[2:]
		}
	case strings.HasPrefix(rest, "-"):
		if !ok {
			v = rest[1:]
		}
	case strings.HasPrefix(rest, ":?"), strings.HasPrefix(rest, "?"):
		empty := strings.HasPrefix(rest, ":")
		if !ok || (empty && v == "") {
			msg := strings.TrimPrefix(strings.TrimPrefix(rest, ":"), "?")
			if msg == "" {
				msg = "is not set"
			}
			return "", []string{name + ": " + msg}, nil
		}
	default:
		return "", nil, fmt.Errorf("bad variable ${%s}", expr)
	}
	return v, nil, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isNameChar(s[i], i == 0) {
			return false
		}
	}
	return true
}

func isNameChar(c byte, first bool) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || !first && c >= '0' && c <= '9'
}
