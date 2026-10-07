package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// strictDecode decodes a mapping node into out, rejecting any key that is not
// a field of out.
//
// yaml.v3's KnownFields setting only reaches the top level document, so a
// section with its own UnmarshalYAML would otherwise accept typos silently and
// keep its default. Since defaults here decide where blobs are written and
// which database is used, a silently ignored key is a production incident, not
// a nit.
//
// extra names keys the caller handles itself, so they count as known.
func strictDecode(node *yaml.Node, out any, extra ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping, got %s", node.Line, kindName(node.Kind))
	}

	known := yamlFieldNames(reflect.TypeOf(out).Elem())
	for _, k := range extra {
		known[k] = struct{}{}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if _, ok := known[key.Value]; !ok {
			return fmt.Errorf("line %d: unknown field %q, expected one of: %s",
				key.Line, key.Value, strings.Join(sortedKeys(known), ", "))
		}
	}

	if err := node.Decode(out); err != nil {
		return cleanYAMLError(err)
	}
	return nil
}

// yamlFieldNames lists the keys a struct accepts, taking the yaml tag as the
// single source of truth so that a renamed field cannot drift from its
// validation.
func yamlFieldNames(t reflect.Type) map[string]struct{} {
	names := make(map[string]struct{}, t.NumField())
	for i := range t.NumField() {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue // unexported
		}

		name := strings.ToLower(field.Name)
		if tag, ok := field.Tag.Lookup("yaml"); ok {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
		}
		names[name] = struct{}{}
	}
	return names
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "a document"
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a value"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "something unrecognised"
	}
}
