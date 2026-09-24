package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Listen describes the HTTP listener.
//
// Short form: `listen: :8080`. Long form: a mapping with any of the fields
// below. Both produce the same struct.
type Listen struct {
	Address           string   `yaml:"address"`
	ReadHeaderTimeout Duration `yaml:"readHeaderTimeout"`
	ShutdownTimeout   Duration `yaml:"shutdownTimeout"`
}

func defaultListen() Listen {
	return Listen{
		Address: ":8080",
		// A slow-loris client must not hold a connection open indefinitely,
		// and a registry is an attractive place to try it.
		ReadHeaderTimeout: Duration(10 * time.Second),
		ShutdownTimeout:   Duration(20 * time.Second),
	}
}

// UnmarshalYAML accepts the address on its own or the full mapping.
func (l *Listen) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		l.Address = node.Value
		return nil
	}

	type raw Listen
	return strictDecode(node, (*raw)(l))
}

func (l *Listen) applyDefaults() {
	d := defaultListen()
	if l.Address == "" {
		l.Address = d.Address
	}
	if l.ReadHeaderTimeout == 0 {
		l.ReadHeaderTimeout = d.ReadHeaderTimeout
	}
	if l.ShutdownTimeout == 0 {
		l.ShutdownTimeout = d.ShutdownTimeout
	}
}

func (l *Listen) validate() error {
	if strings.TrimSpace(l.Address) == "" {
		return fmt.Errorf("listen.address: must not be empty")
	}
	return nil
}

// Backend names a storage implementation.
type Backend string

const (
	// BackendFilesystem stores blobs in a local directory.
	BackendFilesystem Backend = "filesystem"
	// BackendS3 stores blobs in an S3 compatible bucket.
	BackendS3 Backend = "s3"
)

// Storage describes where blobs are written.
//
// Short form: `storage: ./data` or `storage: s3://bucket/prefix`. Long form: a
// mapping naming the backend and its settings. Credentials are better left out
// of the file entirely, in which case the S3 backend uses the ambient AWS
// credential chain.
type Storage struct {
	Backend Backend `yaml:"backend"`

	// Filesystem
	Path string `yaml:"path"`

	// S3
	Bucket          string `yaml:"bucket"`
	Prefix          string `yaml:"prefix"`
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	PathStyle       bool   `yaml:"pathStyle"`
	AccessKeyID     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	SessionToken    string `yaml:"sessionToken"`
}

func defaultStorage() Storage {
	return Storage{Backend: BackendFilesystem, Path: "./data"}
}

// UnmarshalYAML accepts a path, an s3:// URL, or the full mapping.
func (s *Storage) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return s.desugar(node)
	}

	type raw Storage
	return strictDecode(node, (*raw)(s))
}

// desugar expands the scalar form. It is one-way and total: every scalar has a
// mapping that means exactly the same thing, and nothing downstream can tell
// which was written. See ADR 7.
func (s *Storage) desugar(node *yaml.Node) error {
	value := strings.TrimSpace(node.Value)
	if value == "" {
		return fmt.Errorf("line %d: storage must not be empty", node.Line)
	}

	if !strings.HasPrefix(value, "s3://") {
		s.Backend = BackendFilesystem
		s.Path = value
		return nil
	}

	rest := strings.TrimPrefix(value, "s3://")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return fmt.Errorf("line %d: storage %q names no bucket: write s3://bucket or s3://bucket/prefix", node.Line, value)
	}

	s.Backend = BackendS3
	s.Bucket = bucket
	s.Prefix = strings.Trim(prefix, "/")
	return nil
}

func (s *Storage) applyDefaults() {
	if s.Backend == "" {
		s.Backend = BackendFilesystem
	}
	if s.Backend == BackendFilesystem && s.Path == "" {
		s.Path = defaultStorage().Path
	}
	if s.Backend == BackendS3 && s.Region == "" {
		// S3 compatible stores ignore the region but the SDK insists on one.
		s.Region = "us-east-1"
	}
}

func (s *Storage) validate() error {
	switch s.Backend {
	case BackendFilesystem:
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("storage.path: required for the %s backend", s.Backend)
		}
		for name, set := range map[string]bool{
			"bucket":   s.Bucket != "",
			"prefix":   s.Prefix != "",
			"endpoint": s.Endpoint != "",
			// region is defaulted, never rejected
			"pathStyle":       s.PathStyle,
			"accessKeyId":     s.AccessKeyID != "",
			"secretAccessKey": s.SecretAccessKey != "",
		} {
			if set {
				return fmt.Errorf("storage.%s: not valid for the %s backend", name, s.Backend)
			}
		}
		return nil

	case BackendS3:
		if strings.TrimSpace(s.Bucket) == "" {
			return fmt.Errorf("storage.bucket: required for the %s backend", s.Backend)
		}
		if s.Path != "" {
			return fmt.Errorf("storage.path: not valid for the %s backend, use prefix", s.Backend)
		}
		if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
			return fmt.Errorf("storage.accessKeyId and storage.secretAccessKey: set both or neither")
		}
		return nil

	default:
		return fmt.Errorf("storage.backend: %q is not a backend, want %s or %s", s.Backend, BackendFilesystem, BackendS3)
	}
}

// Database describes the Postgres connection.
//
// Short form: `database: postgres://...`. Long form: a mapping with the URL and
// pool settings. Postgres is the only supported store; see ADR 2.
type Database struct {
	URL            string   `yaml:"url"`
	MaxConns       int32    `yaml:"maxConns"`
	MinConns       int32    `yaml:"minConns"`
	ConnectTimeout Duration `yaml:"connectTimeout"`
}

func defaultDatabase() Database {
	// G101: this is the development-only credential from
	// deploy/compose/docker-compose.yml, published in CLAUDE.md, and the
	// reason `make up` needs no configuration file. It must never appear in a
	// deployment manifest, which is a documented rule rather than a secret.
	return Database{ //nolint:gosec
		// Matches deploy/compose/docker-compose.yml, so a developer who ran
		// `make up` needs no configuration file at all.
		URL:            "postgres://sindri:sindri@127.0.0.1:55432/sindri?sslmode=disable",
		MaxConns:       10,
		MinConns:       0,
		ConnectTimeout: Duration(10 * time.Second),
	}
}

// UnmarshalYAML accepts the URL on its own or the full mapping.
func (d *Database) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		d.URL = node.Value
		return nil
	}

	type raw Database
	return strictDecode(node, (*raw)(d))
}

func (d *Database) applyDefaults() {
	def := defaultDatabase()
	if d.URL == "" {
		d.URL = def.URL
	}
	if d.MaxConns == 0 {
		d.MaxConns = def.MaxConns
	}
	if d.ConnectTimeout == 0 {
		d.ConnectTimeout = def.ConnectTimeout
	}
}

func (d *Database) validate() error {
	parsed, err := url.Parse(d.URL)
	if err != nil {
		return fmt.Errorf("database.url: %w", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("database.url: scheme %q is not supported, Sindri stores metadata in Postgres", parsed.Scheme)
	}
	if d.MaxConns < 1 {
		return fmt.Errorf("database.maxConns: must be at least 1")
	}
	if d.MinConns < 0 || d.MinConns > d.MaxConns {
		return fmt.Errorf("database.minConns: must be between 0 and maxConns (%d)", d.MaxConns)
	}
	return nil
}

// Redacted returns the database URL with any password replaced, for logging.
// A connection string is the single most likely credential to end up in a log
// line. See CLAUDE.md's fourth security rule.
func (d Database) Redacted() string {
	parsed, err := url.Parse(d.URL)
	if err != nil {
		return "postgres://invalid"
	}
	return parsed.Redacted()
}

// Log describes the structured logger.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func defaultLog() Log { return Log{Level: "info", Format: "json"} }

// UnmarshalYAML accepts the level on its own or the full mapping.
func (l *Log) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		l.Level = node.Value
		return nil
	}

	type raw Log
	return strictDecode(node, (*raw)(l))
}

func (l *Log) applyDefaults() {
	d := defaultLog()
	if l.Level == "" {
		l.Level = d.Level
	}
	if l.Format == "" {
		l.Format = d.Format
	}
}

func (l *Log) validate() error {
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level: %q is not a level, want debug, info, warn, or error", l.Level)
	}
	switch l.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log.format: %q is not a format, want json or text", l.Format)
	}
	return nil
}

// Auth describes how the management API authenticates callers.
//
// Short form: `auth: ${SINDRI_TOKEN}`. Long form: a mapping with static_token.
// M2 is a static bearer placeholder; M7 replaces the authenticator behind the
// same config section and middleware seam (ADR 9: one loader).
type Auth struct {
	StaticToken string `yaml:"static_token"`
}

func defaultAuth() Auth {
	// Development-only default, matching the spirit of the Compose credentials.
	// Production must set auth.static_token (typically via ${SINDRI_TOKEN}).
	return Auth{StaticToken: "sindri-dev-token"}
}

// UnmarshalYAML accepts the token on its own or the full mapping.
func (a *Auth) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		a.StaticToken = node.Value
		return nil
	}

	type raw Auth
	return strictDecode(node, (*raw)(a))
}

func (a *Auth) applyDefaults() {
	if a.StaticToken == "" {
		a.StaticToken = defaultAuth().StaticToken
	}
}

func (a *Auth) validate() error {
	if strings.TrimSpace(a.StaticToken) == "" {
		return fmt.Errorf("auth.static_token: must not be empty")
	}
	return nil
}
