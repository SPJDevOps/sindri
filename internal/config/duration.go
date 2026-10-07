package config

import (
	"fmt"
	"strings"
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

// String renders the Go duration form without zero trailing units, so an
// hour prints as 1h rather than 1h0m0s. It still parses with
// time.ParseDuration.
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// MarshalJSON writes the duration as a Go duration string, so the expanded
// manifests a CLI sends to the server read the way they were written.
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// UnmarshalJSON accepts a Go duration string.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	s := string(raw)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("duration %s needs a unit and quotes, for example \"30s\"", s)
	}
	parsed, err := time.ParseDuration(s[1 : len(s)-1])
	if err != nil {
		return fmt.Errorf("%s is not a duration: %w", s, err)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %s must not be negative", s)
	}
	*d = Duration(parsed)
	return nil
}
