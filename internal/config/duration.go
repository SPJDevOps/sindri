package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that must be written with a unit.
//
// A bare 3600 is rejected rather than assumed to be seconds: guessing a unit is
// how a five minute negative cache silently becomes a one hour one. See
// ARCHITECTURE.md section 8.5.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!int" || node.Tag == "!!float" {
		return fmt.Errorf("line %d: duration %s needs a unit, for example %ss or %sm",
			node.Line, node.Value, node.Value, node.Value)
	}

	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration: use a Go duration string such as 30s, 5m, or 1h",
			node.Line, node.Value)
	}
	if parsed < 0 {
		return fmt.Errorf("line %d: duration %s must not be negative", node.Line, node.Value)
	}

	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes the duration back in the form it was accepted in.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the Go duration form.
func (d Duration) String() string { return time.Duration(d).String() }
