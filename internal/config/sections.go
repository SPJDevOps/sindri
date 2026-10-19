package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Listen describes the HTTP listener.
//
// Short form: `listen: :8080` (the default is loopback-only on 127.0.0.1:8080,
// but operators may override it to any bind address). Long form: a mapping with
// any of the fields below. Both produce the same struct.
type Listen struct {
	Address           string   `yaml:"address"`
	ReadHeaderTimeout Duration `yaml:"readHeaderTimeout"`
	ShutdownTimeout   Duration `yaml:"shutdownTimeout"`
}

func defaultListen() Listen {
	return Listen{
		Address: "127.0.0.1:8080",
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
	Path string `yaml:"path,omitempty"`

	// S3
	Bucket          string `yaml:"bucket,omitempty"`
	Prefix          string `yaml:"prefix,omitempty"`
	Region          string `yaml:"region,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"`
	PathStyle       bool   `yaml:"pathStyle,omitempty"`
	AccessKeyID     string `yaml:"accessKeyId,omitempty"`
	SecretAccessKey string `yaml:"secretAccessKey,omitempty"`
	SessionToken    string `yaml:"sessionToken,omitempty"`
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

// Auth describes how callers are authenticated and who is an administrator.
//
// Short form: `auth: ${SINDRI_TOKEN}`, which sets only the bootstrap token.
// Long form: a mapping. See ADR 18.
type Auth struct {
	// StaticToken is the bootstrap administrator credential: what `apply` uses
	// before an identity provider is configured, and the break-glass path
	// after. It is checked in constant time and never logged.
	StaticToken string `yaml:"static_token"`

	// Admins lists identity-provider groups whose members administer the
	// server. Membership is read from the caller's token on every request and
	// never stored (ADR 5).
	Admins []string `yaml:"admins,omitempty"`

	// OIDC configures the identity provider. Absent means only the bootstrap
	// token and machine tokens authenticate.
	OIDC *OIDC `yaml:"oidc,omitempty"`
}

// DevStaticToken is the development default. A server running with it logs a
// warning, because anyone who has read the README holds an admin credential.
const DevStaticToken = "sindri-dev-token" //nolint:gosec // G101: development-only default, documented as such

func defaultAuth() Auth {
	// Parallel to the Compose database credential. Production sets
	// auth.static_token, typically via ${SINDRI_TOKEN}. It must never appear in
	// a deployment manifest.
	return Auth{StaticToken: DevStaticToken}
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
	if a.OIDC != nil {
		a.OIDC.applyDefaults()
	}
}

func (a *Auth) validate() error {
	if strings.TrimSpace(a.StaticToken) == "" {
		return fmt.Errorf("auth.static_token: must not be empty")
	}
	for i, g := range a.Admins {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("auth.admins[%d]: must not be empty", i)
		}
	}
	if a.OIDC != nil {
		return a.OIDC.validate()
	}
	return nil
}

// OIDC configures token validation against an OpenID Connect provider, and
// the browser login flow when RedirectURL is set.
type OIDC struct {
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
	RedirectURL  string `yaml:"redirectUrl"`

	// GroupsClaim names the token claim listing the caller's groups.
	GroupsClaim string `yaml:"groupsClaim"`
	// UsernameClaim names the claim recorded as the actor in the ledger.
	UsernameClaim string `yaml:"usernameClaim"`
	// Scopes requested by the browser login. Default: openid, profile,
	// email. Providers that release groups only on request (Dex: "groups")
	// need it added here.
	Scopes []string `yaml:"scopes,omitempty"`
}

// UnmarshalYAML decodes the mapping, rejecting unknown keys.
func (o *OIDC) UnmarshalYAML(node *yaml.Node) error {
	type raw OIDC
	return strictDecode(node, (*raw)(o))
}

func (o *OIDC) applyDefaults() {
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	if o.UsernameClaim == "" {
		o.UsernameClaim = "email"
	}
}

func (o *OIDC) validate() error {
	if o.Issuer == "" {
		return fmt.Errorf("auth.oidc.issuer: required")
	}
	u, err := url.Parse(o.Issuer)
	if err != nil || u.Host == "" {
		return fmt.Errorf("auth.oidc.issuer: %q is not a URL", o.Issuer)
	}
	plainLoopback := u.Scheme == "http" && isLoopback(u.Hostname())
	if u.Scheme != "https" && !plainLoopback {
		return fmt.Errorf("auth.oidc.issuer: must be https (plain http is accepted only for a loopback address)")
	}
	if o.ClientID == "" {
		return fmt.Errorf("auth.oidc.clientId: required")
	}
	if o.RedirectURL != "" {
		r, err := url.Parse(o.RedirectURL)
		if err != nil || r.Host == "" {
			return fmt.Errorf("auth.oidc.redirectUrl: %q is not a URL", o.RedirectURL)
		}
		// The session cookie is Secure exactly when this is https, so plain
		// http is allowed only where nothing else can see the traffic.
		if r.Scheme != "https" && (r.Scheme != "http" || !isLoopback(r.Hostname())) {
			return fmt.Errorf("auth.oidc.redirectUrl: must be https (plain http is accepted only for a loopback address)")
		}
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func isLoopbackAddress(address string) bool {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return false
	}
	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		return isLoopback(host)
	}
	if strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, "]") {
		candidate := strings.TrimPrefix(trimmed, "[")
		candidate = strings.TrimSuffix(candidate, "]")
		return isLoopback(candidate)
	}
	return isLoopback(trimmed)
}

// Credential is one upstream secret.
type Credential struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// UnmarshalYAML decodes the mapping, rejecting unknown keys.
func (c *Credential) UnmarshalYAML(node *yaml.Node) error {
	type raw Credential
	return strictDecode(node, (*raw)(c))
}

// Credentials maps a reference name to its secret.
type Credentials map[string]Credential

// Names lists the defined references, which is all the reconciler may see.
func (cs Credentials) Names() map[string]struct{} {
	out := make(map[string]struct{}, len(cs))
	for k := range cs {
		out[k] = struct{}{}
	}
	return out
}

func (cs Credentials) validate() error {
	for name, c := range cs {
		if !validRefName(name) {
			return fmt.Errorf("credentials.%s: names are lowercase letters, digits, '-', and '_'", name)
		}
		if c.Password == "" {
			return fmt.Errorf("credentials.%s.password: required", name)
		}
	}
	return nil
}

func validRefName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Worker configures the background worker and the jobs it schedules (ADR 20).
//
// Short form: `worker: false` turns it off, `worker: true` is the default.
// Long form: a mapping with any of the fields below.
type Worker struct {
	// Enabled runs a worker inside `serve`. A server with it off still
	// enqueues work, such as `sindri gc`, for a worker elsewhere to claim.
	Enabled   bool            `yaml:"enabled"`
	GC        WorkerGC        `yaml:"gc"`
	Retention WorkerRetention `yaml:"retention"`
}

// WorkerGC configures garbage collection (ADR 19).
type WorkerGC struct {
	// Interval schedules collection. Zero, the default, never schedules it:
	// "untagged and unpromoted is garbage" is a policy to adopt after a dry
	// run, not one to inherit by upgrading. `sindri gc` works either way.
	Interval Duration `yaml:"interval"`
	// Grace protects every blob seen more recently than this, whatever
	// references it.
	Grace Duration `yaml:"grace"`
}

// WorkerRetention configures the cleanup jobs.
type WorkerRetention struct {
	Interval Duration `yaml:"interval"`
	// UploadMaxIdle is how long a staged upload may go unwritten before it
	// is discarded.
	UploadMaxIdle Duration `yaml:"uploadMaxIdle"`
	// ProxyStaleAfter is how long past its TTL a proxy cache entry is kept to
	// serve through an upstream outage before retention evicts it.
	ProxyStaleAfter Duration `yaml:"proxyStaleAfter"`
}

func defaultWorker() Worker {
	return Worker{
		Enabled: true,
		GC:      WorkerGC{Grace: Duration(24 * time.Hour)},
		Retention: WorkerRetention{
			Interval:        Duration(time.Hour),
			UploadMaxIdle:   Duration(24 * time.Hour),
			ProxyStaleAfter: Duration(7 * 24 * time.Hour),
		},
	}
}

// UnmarshalYAML accepts a boolean or the full mapping. In the mapping an
// absent enabled means true, the same as an absent section.
func (w *Worker) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return fmt.Errorf("line %d: worker must be true, false, or a mapping", node.Line)
		}
		*w = Worker{Enabled: enabled}
		return nil
	}

	var raw struct {
		Enabled   *bool           `yaml:"enabled"`
		GC        WorkerGC        `yaml:"gc"`
		Retention WorkerRetention `yaml:"retention"`
	}
	if err := strictDecode(node, &raw); err != nil {
		return err
	}
	*w = Worker{Enabled: raw.Enabled == nil || *raw.Enabled, GC: raw.GC, Retention: raw.Retention}
	return nil
}

// UnmarshalYAML rejects unknown keys.
func (g *WorkerGC) UnmarshalYAML(node *yaml.Node) error {
	type raw WorkerGC
	return strictDecode(node, (*raw)(g))
}

// UnmarshalYAML rejects unknown keys.
func (r *WorkerRetention) UnmarshalYAML(node *yaml.Node) error {
	type raw WorkerRetention
	return strictDecode(node, (*raw)(r))
}

func (w *Worker) applyDefaults() {
	d := defaultWorker()
	if w.GC.Grace == 0 {
		w.GC.Grace = d.GC.Grace
	}
	if w.Retention.Interval == 0 {
		w.Retention.Interval = d.Retention.Interval
	}
	if w.Retention.UploadMaxIdle == 0 {
		w.Retention.UploadMaxIdle = d.Retention.UploadMaxIdle
	}
	if w.Retention.ProxyStaleAfter == 0 {
		w.Retention.ProxyStaleAfter = d.Retention.ProxyStaleAfter
	}
}

func (w *Worker) validate() error {
	if w.Retention.UploadMaxIdle.Duration() < time.Minute {
		return fmt.Errorf("worker.retention.uploadMaxIdle: %s would discard uploads clients are still sending; use at least 1m",
			w.Retention.UploadMaxIdle)
	}
	return nil
}
