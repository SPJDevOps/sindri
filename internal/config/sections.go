package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spj/sindri/internal/model"
)

// Listen describes the HTTP listener.
//
// Short form: `listen: :8080`. Long form: a mapping with any of the fields
// below. Both produce the same struct.
//
// A field's first comment paragraph is its meaning in docs/configuration.md,
// which TestReference_PageMatchesStructs keeps in step with these structs. It
// is written for an operator; later paragraphs are for the code's readers.
type Listen struct {
	// Address the HTTP server binds. Must not be empty.
	Address string `yaml:"address"`
	// How long a client may take to send request headers.
	ReadHeaderTimeout Duration `yaml:"readHeaderTimeout"`
	// How long shutdown waits for requests in flight.
	ShutdownTimeout Duration `yaml:"shutdownTimeout"`
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
	// `filesystem` or `s3`.
	Backend Backend `yaml:"backend"`

	// Directory for blobs. Filesystem only, and required there. A relative
	// path is resolved against the configuration file's directory.
	Path string `yaml:"path,omitempty"`

	// Bucket name. S3 only, and required there.
	Bucket string `yaml:"bucket,omitempty"`
	// Key prefix inside the bucket. S3 only.
	Prefix string `yaml:"prefix,omitempty"`
	// S3 region. Compatible stores ignore it, but the SDK needs one.
	// Default: `us-east-1` on S3.
	Region string `yaml:"region,omitempty"`
	// URL of an S3-compatible store such as MinIO. S3 only. Default: unset,
	// which is AWS.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Use path-style addressing, which most S3-compatible stores need. S3
	// only.
	PathStyle bool `yaml:"pathStyle,omitempty"`
	// Static access key. Set it together with `secretAccessKey`, or leave
	// both out to use the ambient AWS credential chain.
	AccessKeyID string `yaml:"accessKeyId,omitempty"`
	// Static secret key. Redacted by `config show`.
	SecretAccessKey string `yaml:"secretAccessKey,omitempty"`
	// Session token for temporary credentials. Redacted by `config show`.
	SessionToken string `yaml:"sessionToken,omitempty"`
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
	// Connection URL. The scheme must be `postgres` or `postgresql`. The
	// password is redacted in logs and by `config show`. The default is the
	// development stack's, with a development-only credential: never deploy
	// it.
	URL string `yaml:"url"`
	// Pool size. At least 1.
	MaxConns int32 `yaml:"maxConns"`
	// Connections kept open. Between 0 and `maxConns`.
	MinConns int32 `yaml:"minConns"`
	// How long to wait for a connection.
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
	// `debug`, `info`, `warn`, or `error`.
	Level string `yaml:"level"`
	// `json` or `text`.
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
	// The bootstrap administrator credential: what `sindri apply` uses before
	// an identity provider is configured, and the break-glass path after.
	// Compared in constant time, never logged, and redacted by `config show`.
	// The default logs a warning while it is in use.
	StaticToken string `yaml:"static_token"`

	// Identity-provider groups whose members administer the server. Entries
	// must not be empty. Membership is read from the caller's token on each
	// request and never stored.
	//
	// See ADR 5.
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
	// The provider's issuer URL. Required. Must be `https`, or `http` for a
	// loopback address.
	Issuer string `yaml:"issuer"`
	// OAuth client id. Required.
	ClientID string `yaml:"clientId"`
	// OAuth client secret. Redacted by `config show`.
	ClientSecret string `yaml:"clientSecret"`
	// The browser login callback, for example
	// `https://sindri.example.com/auth/callback`. Setting it enables UI
	// sign-in. Same scheme rule as `issuer`. The session cookie is `Secure`
	// exactly when this is `https`.
	RedirectURL string `yaml:"redirectUrl"`

	// Token claim listing the caller's groups.
	GroupsClaim string `yaml:"groupsClaim"`
	// Token claim recorded as the actor in the audit ledger.
	UsernameClaim string `yaml:"usernameClaim"`
	// Scopes the browser login requests. Providers that release groups only
	// on request, such as Dex, need `groups` added. Default: `openid`,
	// `profile`, `email`.
	//
	// The default is applied by internal/auth, not here, so `config show`
	// prints only what the file wrote.
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
	if _, err := parseHTTPSOrLoopback("auth.oidc.issuer", o.Issuer); err != nil {
		return err
	}
	if o.ClientID == "" {
		return fmt.Errorf("auth.oidc.clientId: required")
	}
	if o.RedirectURL != "" {
		// The session cookie is Secure exactly when this is https, so plain
		// http is allowed only where nothing else can see the traffic.
		if _, err := parseHTTPSOrLoopback("auth.oidc.redirectUrl", o.RedirectURL); err != nil {
			return err
		}
	}
	return nil
}

// parseHTTPSOrLoopback parses a URL setting, accepting https anywhere and
// plain http only for a loopback address. Errors name the field.
func parseHTTPSOrLoopback(field, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s: %q is not a URL", field, raw)
	}
	if err := checkHTTPSOrLoopback(field, u); err != nil {
		return nil, err
	}
	return u, nil
}

// checkHTTPSOrLoopback applies the scheme rule to a parsed URL. Its error
// never includes the URL.
func checkHTTPSOrLoopback(field string, u *url.URL) error {
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return fmt.Errorf("%s: must be https (plain http is accepted only for a loopback address)", field)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Credential is one upstream secret.
type Credential struct {
	// Upstream user name. Redacted by `config show`.
	Username string `yaml:"username"`
	// Upstream password, sent as HTTP Basic with `username`. Set it, or `token`,
	// never both. Redacted by `config show`.
	Password string `yaml:"password"`
	// Upstream token, sent as HTTP Bearer. Set it instead of `username` and
	// `password`. An OCI proxy cannot use one. Redacted by `config show`.
	Token string `yaml:"token"`
}

// Kind says how the credential authenticates.
func (c Credential) Kind() model.CredentialKind {
	if c.Token != "" {
		return model.CredentialToken
	}
	return model.CredentialBasic
}

// UnmarshalYAML decodes the mapping, rejecting unknown keys.
func (c *Credential) UnmarshalYAML(node *yaml.Node) error {
	type raw Credential
	return strictDecode(node, (*raw)(c))
}

// Credentials maps a reference name to its secret.
type Credentials map[string]Credential

// Names maps each defined reference to how it authenticates. A kind is not a
// secret, so this is all the reconciler may see.
func (cs Credentials) Names() map[string]model.CredentialKind {
	out := make(map[string]model.CredentialKind, len(cs))
	for k, c := range cs {
		out[k] = c.Kind()
	}
	return out
}

func (cs Credentials) validate() error {
	for name, c := range cs {
		if !validRefName(name) {
			return fmt.Errorf("credentials.%s: names are lowercase letters, digits, '-', and '_'", name)
		}
		if c.Token != "" && (c.Password != "" || c.Username != "") {
			return fmt.Errorf("credentials.%s: set token, or username and password, not both", name)
		}
		if c.Token == "" && c.Password == "" {
			return fmt.Errorf("credentials.%s: set token, or username and password", name)
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
	// Run a worker inside `serve`. With it off, the server still enqueues
	// work, such as `sindri gc`, for a worker elsewhere to claim. An absent
	// `enabled` in the mapping means `true`.
	Enabled   bool            `yaml:"enabled"`
	GC        WorkerGC        `yaml:"gc"`
	Retention WorkerRetention `yaml:"retention"`
}

// WorkerGC configures garbage collection (ADR 19).
type WorkerGC struct {
	// How often to schedule garbage collection. Unset never schedules it:
	// "untagged and unpromoted is garbage" is a policy to adopt after a dry
	// run, not one to inherit by upgrading. `sindri gc` works either way.
	Interval Duration `yaml:"interval"`
	// Blobs seen more recently than this are kept, whatever references them.
	Grace Duration `yaml:"grace"`
}

// WorkerRetention configures the cleanup jobs.
type WorkerRetention struct {
	// How often the cleanup jobs run.
	Interval Duration `yaml:"interval"`
	// How long a staged upload may go unwritten before it is discarded. At
	// least `1m`.
	UploadMaxIdle Duration `yaml:"uploadMaxIdle"`
	// How long past its TTL a proxy cache entry is kept to serve through an
	// upstream outage, before retention evicts it. Also how long a cached
	// file that upstream no longer lists is kept before its row is removed.
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

// Scanning configures vulnerability scanning of hosted publishes (ADR 23, ADR 29).
//
// Short form: `scanning: true` turns it on with the defaults below, and
// `scanning: false` is the default. Long form: a mapping with any of the
// fields below.
type Scanning struct {
	// Scan hosted publishes for known vulnerabilities and attach the result
	// to the artifact. Off when the section is absent. An absent `enabled` in
	// the mapping means `true`, because writing the section signals intent.
	Enabled bool `yaml:"enabled"`
	// The Trivy executable. A bare name is looked up on the PATH. Sindri runs
	// it as a separate process and never links it in.
	Trivy string `yaml:"trivy"`
	// A mirror of the Trivy vulnerability database, passed as
	// `--db-repository`, for a server that cannot reach the public one. Cannot
	// be combined with `skipDBUpdate`.
	DBRepository string `yaml:"dbRepository"`
	// A mirror of Trivy's Java vulnerability database, passed as
	// `--java-db-repository`. Trivy fetches that database separately, and only
	// when a scanned image or package holds a jar. An air-gapped install that
	// scans jar-containing images sets this, or pre-seeds the Java database in
	// `cacheDir`; otherwise, with `skipDBUpdate`, such a scan fails (fail
	// closed). Cannot be combined with `skipDBUpdate`.
	JavaDBRepository string `yaml:"javaDbRepository"`
	// Where Trivy keeps its database and caches between scans. Empty uses
	// `sindri/trivy` under the user's cache directory (`$XDG_CACHE_HOME`, or
	// `~/.cache` on Linux). The `-trivy` release image sets
	// `/var/cache/sindri-trivy`.
	CacheDir string `yaml:"cacheDir"`
	// How long one scan may run before it is cancelled and retried. At least
	// `1m`.
	Timeout Duration `yaml:"timeout"`
	// Never download the vulnerability database, and scan with the one already
	// in `cacheDir`, including the Java database that Trivy fetches separately
	// when it finds a jar. For an air-gapped server with a pre-seeded cache; a
	// jar-containing subject fails to scan if the Java database is not in it.
	// Cannot be combined with `dbRepository`.
	SkipDBUpdate bool `yaml:"skipDBUpdate"`
	// The most bytes unpacked from one wheel or tarball while scanning it, so
	// that an archive bomb cannot fill the disk. At least 1048576 (1 MiB).
	MaxUnpackedBytes int64 `yaml:"maxUnpackedBytes"`
}

const minScanUnpackedBytes = 1 << 20

func defaultScanning() Scanning {
	return Scanning{
		Trivy:            "trivy",
		Timeout:          Duration(10 * time.Minute),
		MaxUnpackedBytes: 1 << 30,
	}
}

// UnmarshalYAML accepts a boolean or the full mapping. In the mapping an
// absent enabled means true; an absent section means false.
func (s *Scanning) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return fmt.Errorf("line %d: scanning must be true, false, or a mapping", node.Line)
		}
		*s = Scanning{Enabled: enabled}
		return nil
	}

	var raw struct {
		Enabled          *bool    `yaml:"enabled"`
		Trivy            string   `yaml:"trivy"`
		DBRepository     string   `yaml:"dbRepository"`
		JavaDBRepository string   `yaml:"javaDbRepository"`
		CacheDir         string   `yaml:"cacheDir"`
		Timeout          Duration `yaml:"timeout"`
		SkipDBUpdate     bool     `yaml:"skipDBUpdate"`
		MaxUnpackedBytes int64    `yaml:"maxUnpackedBytes"`
	}
	if err := strictDecode(node, &raw); err != nil {
		return err
	}
	*s = Scanning{
		Enabled:          raw.Enabled == nil || *raw.Enabled,
		Trivy:            raw.Trivy,
		DBRepository:     raw.DBRepository,
		JavaDBRepository: raw.JavaDBRepository,
		CacheDir:         raw.CacheDir,
		Timeout:          raw.Timeout,
		SkipDBUpdate:     raw.SkipDBUpdate,
		MaxUnpackedBytes: raw.MaxUnpackedBytes,
	}
	return nil
}

func (s *Scanning) applyDefaults() {
	d := defaultScanning()
	if s.Trivy == "" {
		s.Trivy = d.Trivy
	}
	if s.Timeout == 0 {
		s.Timeout = d.Timeout
	}
	if s.MaxUnpackedBytes == 0 {
		s.MaxUnpackedBytes = d.MaxUnpackedBytes
	}
}

// ResolvedCacheDir is the directory Trivy keeps its database in: CacheDir, or
// `sindri/trivy` under the user's cache directory when that is empty. It does
// not create the directory or check who can write to it; the scanner does.
func (s Scanning) ResolvedCacheDir() (string, error) {
	if s.CacheDir != "" {
		return s.CacheDir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("scanning.cacheDir is unset and the user cache directory is unknown: %w", err)
	}
	return filepath.Join(base, "sindri", "trivy"), nil
}

// validate checks the values whether or not scanning is on: a bad value is a
// bad value. It does not look for the executable; `sindri config check` does.
func (s *Scanning) validate() error {
	if s.Timeout.Duration() < time.Minute {
		return fmt.Errorf("scanning.timeout: %s would cancel scans that are still working; use at least 1m", s.Timeout)
	}
	if s.MaxUnpackedBytes < minScanUnpackedBytes {
		return fmt.Errorf("scanning.maxUnpackedBytes: %d is below the 1048576 (1 MiB) minimum", s.MaxUnpackedBytes)
	}
	if s.SkipDBUpdate && s.JavaDBRepository != "" {
		return fmt.Errorf("scanning.javaDbRepository: cannot be set with skipDBUpdate, which never downloads the Java database")
	}
	if s.SkipDBUpdate && s.DBRepository != "" {
		return fmt.Errorf("scanning.dbRepository: cannot be set with skipDBUpdate, which never downloads the database")
	}
	return nil
}
