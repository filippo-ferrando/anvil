// Package yamlcheck holds what the YAML validators (cloud-init, intent manifests) share:
// the Problem they report and syntax errors with a usable line number.
package yamlcheck

import (
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Problem is one finding in a YAML document. Line is 1-based, 0 when it has no line.
type Problem struct {
	Line    int
	Message string
	Warning bool // the document still works, like an unknown cloud-init key
}

var lineRe = regexp.MustCompile(`line (\d+): `)

// Line returns the line number in an error like "yaml: line 4: ...", or 0.
func Line(err error) int {
	m := lineRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// Message is err without the "yaml:" and "line N:" prefixes, since the line is shown apart.
func Message(err error) string {
	msg := err.Error()
	if loc := lineRe.FindAllStringIndex(msg, -1); loc != nil {
		return msg[loc[len(loc)-1][1]:]
	}
	return strings.TrimPrefix(msg, "yaml: ")
}

// Syntax reports a YAML syntax error, or nil. yaml.v3 often reports the line before the
// enclosing block, so the line is the first one where the text up to it stops parsing.
// ponytail: a multi-line flow list or quoted string before the error can be blamed instead.
func Syntax(data string) *Problem {
	var v any
	err := yaml.Unmarshal([]byte(data), &v)
	if err == nil {
		return nil
	}
	msg := Message(err)
	lines := strings.SplitAfter(data, "\n")
	for n := 1; n <= len(lines); n++ {
		if yaml.Unmarshal([]byte(strings.Join(lines[:n], "")), &v) != nil {
			return &Problem{Line: n, Message: msg}
		}
	}
	return &Problem{Line: Line(err), Message: msg}
}
