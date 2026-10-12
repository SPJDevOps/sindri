// Package config loads sindri.yaml.
//
// There is one configuration file and one loader for the life of the project.
// M1 defines the listen, storage, database, and log sections; later milestones
// add repos, lifecycle, and defaults as fields on the same structs. A second
// configuration system, or a separate simplified file for small deployments,
// is the failure this package exists to prevent. See ARCHITECTURE.md section 8
// and ADR 7.
//
// Every section accepts a short form and a long form, and the short form
// desugars into the long form totally: nothing downstream may ask which
// spelling was written. Unknown keys are an error, so a typo fails at load
// rather than silently keeping a default.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the file `sindri serve` reads when --config is not given. Its
// absence is not an error: a server with no configuration at all runs on the
// defaults in this package, which match the development Compose stack.
const DefaultPath = "sindri.yaml"

// Config is the whole configuration tree, after desugaring and defaulting.
type Config struct {
	Listen   Listen   `yaml:"listen"`
	Storage  Storage  `yaml:"storage"`
	Database Database `yaml:"database"`
	Log      Log      `yaml:"log"`
	Auth     Auth     `yaml:"auth"`
	Worker   Worker   `yaml:"worker"`

	// Credentials holds upstream secrets that proxy repositories name by key.
	// Values come from ${VAR} substitution; a secret is never written to the
	// database or printed by `config show` (ADR 15).
	Credentials Credentials `yaml:"credentials,omitempty"`

	// Desired is the repos, lifecycle, and defaults sections, desugared into
	// the long form. Nil when the file declares none of them, in which case
	// `serve` does not reconcile and repositories stay API-managed.
	Desired *Manifests `yaml:"-"`
}

// Default returns the configuration a server runs on with no file present.
func Default() Config {
	return Config{
		Listen:   defaultListen(),
		Storage:  defaultStorage(),
		Database: defaultDatabase(),
		Log:      defaultLog(),
		Auth:     defaultAuth(),
		Worker:   defaultWorker(),
	}
}

// Load reads and validates the file at path, which must exist.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names their own configuration file
	if err != nil {
		return nil, fmt.Errorf("reading configuration %s: %w", path, err)
	}
	return Parse(raw, path)
}

// LoadOrDefault resolves the configuration the way `serve` does.
//
// An explicit path must exist, because an operator who passed --config and got
// defaults would not find out until something behaved unexpectedly in
// production. With no explicit path, sindri.yaml is used when it is there and
// the built-in defaults when it is not. The returned path is empty when no file
// was read.
func LoadOrDefault(explicitPath string) (*Config, string, error) {
	if explicitPath != "" {
		cfg, err := Load(explicitPath)
		if err != nil {
			return nil, "", err
		}
		return cfg, explicitPath, nil
	}

	if _, err := os.Stat(DefaultPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := Default()
			if err := cfg.Validate(); err != nil {
				return nil, "", err
			}
			return &cfg, "", nil
		}
		return nil, "", fmt.Errorf("looking for %s: %w", DefaultPath, err)
	}

	cfg, err := Load(DefaultPath)
	if err != nil {
		return nil, "", err
	}
	return cfg, DefaultPath, nil
}

// Parse decodes raw, expands environment references, applies defaults, and
// validates. The name is used only to locate errors.
func Parse(raw []byte, name string) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing configuration %s: %w", name, cleanYAMLError(err))
	}

	// An empty file is a valid way to say "everything default".
	if doc.Kind == 0 || len(doc.Content) == 0 {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return &cfg, nil
	}

	if err := expandNode(doc.Content[0], os.LookupEnv); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	// Desired state is desugared from nodes, not decoded into structs, so its
	// errors keep their line numbers for `config check`.
	short := liftShortSections(doc.Content[0])

	// Decode into a zero value and default afterwards, so that a section
	// written in the file replaces its defaults outright rather than merging
	// with them: `storage: s3://bucket` must not keep the default path.
	var cfg Config
	if err := doc.Content[0].Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", name, cleanYAMLError(err))
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if short.present() {
		desired, err := desugarShort(name, short)
		if err != nil {
			return nil, err
		}
		cfg.Desired = desired
	}
	return &cfg, nil
}

// UnmarshalYAML decodes the top level mapping, rejecting unknown sections.
// The desired-state sections were lifted out by Parse before this runs; they
// are named here only so a typo's error message lists them.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	// An absent worker section means a running worker. Every other section's
	// zero value is filled in by applyDefaults, but a false that was written
	// and a false that was never decoded look alike, so this one default is
	// set before decoding instead.
	c.Worker.Enabled = true
	type raw Config
	return strictDecode(node, (*raw)(c), shortSections...)
}

func (c *Config) applyDefaults() {
	c.Listen.applyDefaults()
	c.Storage.applyDefaults()
	c.Database.applyDefaults()
	c.Log.applyDefaults()
	c.Auth.applyDefaults()
	c.Worker.applyDefaults()
}

// Validate reports the first configuration error, naming the field.
func (c *Config) Validate() error {
	for _, check := range []func() error{
		c.Listen.validate,
		c.Storage.validate,
		c.Database.validate,
		c.Log.validate,
		c.Auth.validate,
		c.Worker.validate,
		c.Credentials.validate,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// AbsolutePaths resolves relative filesystem paths against dir, so that a
// configuration file mounted somewhere else still points at the same data
// directory it did when it was written.
func (c *Config) AbsolutePaths(dir string) error {
	if c.Storage.Backend != BackendFilesystem || c.Storage.Path == "" || filepath.IsAbs(c.Storage.Path) {
		return nil
	}

	abs, err := filepath.Abs(filepath.Join(dir, c.Storage.Path))
	if err != nil {
		return fmt.Errorf("resolving storage.path %s: %w", c.Storage.Path, err)
	}
	c.Storage.Path = abs
	return nil
}

// typeSuffix matches the Go type name yaml.v3 appends to its unknown-field and
// type errors. Operators debug their file, not our structs.
var typeSuffix = regexp.MustCompile(` in type [\w.*\[\]]+`)

func cleanYAMLError(err error) error {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		cleaned := make([]string, 0, len(typeErr.Errors))
		for _, e := range typeErr.Errors {
			cleaned = append(cleaned, typeSuffix.ReplaceAllString(e, ""))
		}
		return errors.New(strings.Join(cleaned, "; "))
	}
	return err
}
