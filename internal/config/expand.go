package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// expandNode rewrites ${VAR} and ${VAR:-default} in every string value of the
// tree, so that one file works across development and production.
//
// Expansion happens on parsed scalar nodes rather than on the raw text. A
// password containing ':' or '#' would change the meaning of the document if it
// were spliced in before parsing, and a value expanded before parsing loses the
// line number that an error message needs. Write $$ for a literal dollar sign.
func expandNode(node *yaml.Node, lookup func(string) (string, bool)) error {
	if node == nil {
		return nil
	}

	if node.Kind == yaml.ScalarNode && (node.Tag == "!!str" || node.Tag == "") {
		expanded, err := expandString(node.Value, lookup)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		node.Value = expanded
		node.Tag = "!!str"
		return nil
	}

	for _, child := range node.Content {
		if err := expandNode(child, lookup); err != nil {
			return err
		}
	}
	return nil
}

// expandString performs the substitution on one scalar.
func expandString(s string, lookup func(string) (string, bool)) (string, error) {
	var out strings.Builder

	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			return "", fmt.Errorf("a bare $ in %q: write ${NAME}, ${NAME:-default}, or $$ for a literal dollar", s)
		}

		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated ${ in %q", s)
		}
		ref := s[i+2 : i+end]

		value, err := resolveRef(ref, lookup)
		if err != nil {
			return "", err
		}
		out.WriteString(value)
		i += end + 1
	}

	return out.String(), nil
}

// resolveRef reads one ${...} body.
func resolveRef(ref string, lookup func(string) (string, bool)) (string, error) {
	name, fallback, hasFallback := strings.Cut(ref, ":-")
	if name == "" {
		return "", fmt.Errorf("empty variable name in ${%s}", ref)
	}

	if value, ok := lookup(name); ok {
		return value, nil
	}
	if hasFallback {
		return fallback, nil
	}
	// Defaulting an unset variable to the empty string is how a storage path
	// silently becomes the working directory, so this is an error instead.
	return "", fmt.Errorf("environment variable %s is not set and has no default: write ${%s:-value}", name, name)
}
