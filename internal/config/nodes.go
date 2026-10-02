package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldError is one configuration error located in a file. Errors name the
// file, the line, and the field, because "invalid configuration" sends an
// operator hunting while "sindri.yaml:14: repos.python.members[1]: ..." does
// not (ARCHITECTURE.md section 8.3).
type FieldError struct {
	File  string `json:"file,omitempty"`
	Line  int    `json:"line,omitempty"`
	Field string `json:"field,omitempty"`
	Msg   string `json:"message"`
}

func (e *FieldError) Error() string {
	var b strings.Builder
	if e.File != "" {
		b.WriteString(e.File)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d", e.Line)
		}
		b.WriteString(": ")
	} else if e.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", e.Line)
	}
	if e.Field != "" {
		b.WriteString(e.Field)
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Errors is every problem found in one load or validation pass. `config check`
// reports all of them rather than the first, so a pull request is fixed in one
// round trip instead of one per mistake.
type Errors []*FieldError

func (es Errors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// Err returns nil for an empty list, so callers can return it unconditionally.
func (es Errors) Err() error {
	if len(es) == 0 {
		return nil
	}
	return es
}

// AsErrors flattens err into located errors. An error that carries no location
// becomes one entry with only a message.
func AsErrors(err error) Errors {
	if err == nil {
		return nil
	}
	var es Errors
	if errors.As(err, &es) {
		return es
	}
	var fe *FieldError
	if errors.As(err, &fe) {
		return Errors{fe}
	}
	return Errors{{Msg: err.Error()}}
}

// errs collects located errors while a tree is walked.
type errs struct {
	file string
	list Errors
}

func (c *errs) add(line int, field, format string, args ...any) {
	c.list = append(c.list, &FieldError{File: c.file, Line: line, Field: field, Msg: fmt.Sprintf(format, args...)})
}

// mapping is a decoded YAML mapping that remembers where each key was.
type mapping struct {
	node  *yaml.Node
	keys  []string
	value map[string]*yaml.Node
	line  map[string]int
}

func (m mapping) has(key string) bool {
	_, ok := m.value[key]
	return ok
}

func (m mapping) lineOf(key string) int {
	if l, ok := m.line[key]; ok {
		return l
	}
	return m.node.Line
}

// readMapping checks that n is a mapping whose keys are all in allowed and
// returns it with positions. Unknown keys are reported as errors, matching
// strictDecode's rule for the server sections.
func readMapping(c *errs, n *yaml.Node, field string, allowed ...string) (mapping, bool) {
	n = resolveAlias(n)
	m := mapping{node: n, value: map[string]*yaml.Node{}, line: map[string]int{}}
	if n.Kind != yaml.MappingNode {
		c.add(n.Line, field, "expected a mapping, got %s", kindName(n.Kind))
		return m, false
	}
	known := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		known[k] = struct{}{}
	}
	ok := true
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if key.Value == "<<" {
			// A YAML merge key: fold the referenced mapping's keys in, without
			// overriding keys written explicitly. This is how anchors stand in for
			// the `extends:` the format deliberately lacks (ADR 7).
			for _, src := range mergeSources(val) {
				for j := 0; j+1 < len(src.Content); j += 2 {
					k := src.Content[j].Value
					if _, set := m.value[k]; set || hasKey(n, k) {
						continue
					}
					if !checkKnown(c, known, allowed, src.Content[j], field) {
						ok = false
						continue
					}
					m.keys = append(m.keys, k)
					m.value[k] = src.Content[j+1]
					m.line[k] = src.Content[j].Line
				}
			}
			continue
		}
		if !checkKnown(c, known, allowed, key, field) {
			ok = false
			continue
		}
		if _, dup := m.value[key.Value]; dup {
			c.add(key.Line, join(field, key.Value), "is written twice")
			ok = false
			continue
		}
		m.keys = append(m.keys, key.Value)
		m.value[key.Value] = val
		m.line[key.Value] = key.Line
	}
	return m, ok
}

func checkKnown(c *errs, known map[string]struct{}, allowed []string, key *yaml.Node, field string) bool {
	if known == nil || len(allowed) == 0 {
		return true
	}
	if _, ok := known[key.Value]; ok {
		return true
	}
	sorted := append([]string(nil), allowed...)
	sort.Strings(sorted)
	c.add(key.Line, field, "unknown field %q, expected one of: %s", key.Value, strings.Join(sorted, ", "))
	return false
}

func hasKey(n *yaml.Node, k string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == k {
			return true
		}
	}
	return false
}

func mergeSources(n *yaml.Node) []*yaml.Node {
	n = resolveAlias(n)
	switch n.Kind {
	case yaml.MappingNode:
		return []*yaml.Node{n}
	case yaml.SequenceNode:
		out := make([]*yaml.Node, 0, len(n.Content))
		for _, item := range n.Content {
			if r := resolveAlias(item); r.Kind == yaml.MappingNode {
				out = append(out, r)
			}
		}
		return out
	default:
		return nil
	}
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// readString decodes a scalar string.
func readString(c *errs, n *yaml.Node, field string) (string, bool) {
	n = resolveAlias(n)
	if n.Kind != yaml.ScalarNode {
		c.add(n.Line, field, "expected a value, got %s", kindName(n.Kind))
		return "", false
	}
	return strings.TrimSpace(n.Value), true
}

// readStrings decodes a list of scalar strings.
func readStrings(c *errs, n *yaml.Node, field string) ([]string, bool) {
	n = resolveAlias(n)
	if n.Kind != yaml.SequenceNode {
		c.add(n.Line, field, "expected a list, got %s", kindName(n.Kind))
		return nil, false
	}
	out := make([]string, 0, len(n.Content))
	ok := true
	for i, item := range n.Content {
		s, good := readString(c, item, fmt.Sprintf("%s[%d]", field, i))
		if !good {
			ok = false
			continue
		}
		out = append(out, s)
	}
	return out, ok
}

// readBool decodes a scalar boolean.
func readBool(c *errs, n *yaml.Node, field string) (bool, bool) {
	n = resolveAlias(n)
	var b bool
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		c.add(n.Line, field, "expected true or false, got %q", n.Value)
		return false, false
	}
	if err := n.Decode(&b); err != nil {
		c.add(n.Line, field, "expected true or false, got %q", n.Value)
		return false, false
	}
	return b, true
}

// readDuration decodes a duration with a unit, rejecting bare numbers.
func readDuration(c *errs, n *yaml.Node, field string) (Duration, bool) {
	n = resolveAlias(n)
	var d Duration
	if err := d.UnmarshalYAML(n); err != nil {
		// The Duration error already carries "line N:"; strip it so the
		// located error does not say the line twice.
		msg := err.Error()
		if _, rest, found := strings.Cut(msg, ": "); found && strings.HasPrefix(msg, "line ") {
			msg = rest
		}
		c.add(n.Line, field, "%s", msg)
		return 0, false
	}
	return d, true
}

func join(field, key string) string {
	if field == "" {
		return key
	}
	return field + "." + key
}
