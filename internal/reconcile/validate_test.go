package reconcile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/reconcile"
)

func TestValidateRepository_LifecycleOnlyOnHosted(t *testing.T) {
	tests := []struct {
		name    string
		typ     model.RepoType
		wantErr bool
	}{
		{"Proxy", model.RepoTypeProxy, true},
		{"Virtual", model.RepoTypeVirtual, true},
		{"Hosted", model.RepoTypeHosted, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := reconcile.ValidateRepository(reconcile.RepositorySpec{
				Name: "r", Type: tc.typ, Protocol: model.ProtocolPyPI, LifecycleStages: []string{"prod"},
			}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "I4") {
				t.Fatalf("error should name I4: %v", err)
			}
		})
	}
}

// ADR 15: an unknown credentials reference is refused when the repository is
// written, never discovered at pull time. A known one is accepted.
func TestValidateRepository_CredentialsMustExist(t *testing.T) {
	known := map[string]model.CredentialKind{"mirror": model.CredentialBasic}
	proxy := reconcile.RepositorySpec{
		Name: "p", Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI,
		UpstreamURL: "https://registry.example.com",
	}

	proxy.CredentialsRef = "mirror"
	if err := reconcile.ValidateRepository(proxy, known); err != nil {
		t.Fatalf("known reference refused: %v", err)
	}
	proxy.CredentialsRef = "fancysecret"
	if err := reconcile.ValidateRepository(proxy, known); !errors.Is(err, reconcile.ErrUnknownCredentials) {
		t.Fatalf("unknown reference: want ErrUnknownCredentials, got %v", err)
	}
	hosted := reconcile.RepositorySpec{Name: "h", Type: model.RepoTypeHosted, Protocol: model.ProtocolPyPI, CredentialsRef: "mirror"}
	if err := reconcile.ValidateRepository(hosted, known); err == nil {
		t.Fatal("credentials on a hosted repository were accepted")
	}
}

func TestValidateSet_GateFixturesAreValid(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "testdata", "manifests"),
	} {
		m, err := config.LoadManifests(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := reconcile.ValidateSet(m, reconcile.ValidateOptions{}); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "config", "sindri.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile.ValidateSet(cfg.Desired, reconcile.ValidateOptions{Credentials: cfg.Credentials.Names()}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSet_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		docs  string
		opts  reconcile.ValidateOptions
		wants []string
	}{
		{
			// Gate test 6, statically: invariant I4 at write time.
			name: "LifecycleOnProxy",
			docs: `
kind: Repository
metadata: { name: pypi }
spec:
  type: proxy
  protocol: pypi
  upstream: { url: https://pypi.org/simple, ttl: 1h, negativeTtl: 5m }
  lifecycle: { stages: [prod] }
`,
			wants: []string{"m.yaml:8:", "Repository[pypi].spec.lifecycle", "invariant I4"},
		},
		{
			// Gate test 2 in the long form: the members' protocols disagree.
			name: "VirtualMixesProtocols",
			docs: `
kind: Repository
metadata: { name: py }
spec: { type: hosted, protocol: pypi }
---
kind: Repository
metadata: { name: img }
spec: { type: hosted, protocol: oci }
---
kind: Repository
metadata: { name: all }
spec:
  type: virtual
  protocol: pypi
  members:
    - ref: hosted/py
    - ref: hosted/img
`,
			wants: []string{"m.yaml:17:", "Repository[all].spec.members[1]", "one wire protocol"},
		},
		{
			name: "MemberTypeIsWrong",
			docs: `
kind: Repository
metadata: { name: py }
spec: { type: hosted, protocol: pypi }
---
kind: Repository
metadata: { name: v }
spec: { type: virtual, protocol: pypi, members: [{ ref: proxy/py }] }
`,
			wants: []string{"says proxy", "is a hosted repository"},
		},
		{
			name: "UnknownCredentials",
			docs: `
kind: Repository
metadata: { name: p }
spec:
  type: proxy
  protocol: oci
  upstream: { url: https://r.example.com, ttl: 1h, negativeTtl: 1m, credentials: fancysecret }
`,
			opts:  reconcile.ValidateOptions{Credentials: map[string]model.CredentialKind{}},
			wants: []string{"upstream.credentials", "fancysecret", "not defined"},
		},
		{
			name: "UndefinedStage",
			docs: `
kind: Lifecycle
spec: { stages: [{ name: dev }] }
---
kind: Repository
metadata: { name: h }
spec: { type: hosted, protocol: oci, lifecycle: { stages: [prod] } }
`,
			wants: []string{`stage "prod" is not defined`},
		},
		{
			name: "Cycle",
			docs: `
kind: Repository
metadata: { name: a }
spec: { type: virtual, protocol: oci, members: [{ ref: virtual/b }] }
---
kind: Repository
metadata: { name: b }
spec: { type: virtual, protocol: oci, members: [{ ref: virtual/a }] }
`,
			wants: []string{"contains itself"},
		},
		{
			name: "AnonymousGrant",
			docs: `
kind: Repository
metadata: { name: h }
spec: { type: hosted, protocol: oci, access: { read: [anonymous] } }
`,
			wants: []string{"access.read", "no anonymous access"},
		},
		{
			name: "BareRefDoesNotLeaveTheNamespace",
			docs: `
kind: Repository
metadata: { name: base }
spec: { type: hosted, protocol: oci }
---
kind: Repository
metadata: { name: v, namespace: payments }
spec: { type: virtual, protocol: oci, members: [{ ref: hosted/base }] }
`,
			wants: []string{`resolves to repository "payments/base"`},
		},
		{
			name: "CentralizedRejectsTeamRepositories",
			docs: `
kind: OrgPolicy
spec: { mode: centralized }
---
kind: Repository
metadata: { name: h, namespace: payments }
spec: { type: hosted, protocol: oci }
`,
			wants: []string{"centralized"},
		},
		{
			name: "HybridRequiresMandatoryRepositories",
			docs: `
kind: OrgPolicy
spec:
  mode: hybrid
  mandatoryRepos: [{ name: approved-base-images, locked: true }]
`,
			wants: []string{`mandatory repository "approved-base-images" is not declared`},
		},
		{
			name: "ReservedNamespace",
			docs: `
kind: Repository
metadata: { name: h, namespace: org }
spec: { type: hosted, protocol: oci }
`,
			wants: []string{"reserved"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := config.ParseManifests([]byte(tc.docs), "m.yaml")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = reconcile.ValidateSet(m, tc.opts)
			if !errors.Is(err, reconcile.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

// Cross-boundary references resolve the way ADR 17 describes, and only a
// prefix crosses a boundary.
func TestResolveRef(t *testing.T) {
	exists := func(n string) bool { return n == "payments/team/lib" }
	tests := []struct{ ns, ref, want string }{
		{"", "internal", "internal"},
		{"", "org/internal", "internal"},
		{"payments", "internal", "payments/internal"},
		{"payments", "org/base", "base"},
		{"payments", "search/index", "search/index"},
		{"payments", "team/lib", "payments/team/lib"},
	}
	for _, tc := range tests {
		if got := reconcile.ResolveRef(tc.ns, tc.ref, exists); got != tc.want {
			t.Errorf("ResolveRef(%q, %q) = %q, want %q", tc.ns, tc.ref, got, tc.want)
		}
	}
}

// Every problem is reported, each located.
func TestValidateSet_ReportsEveryErrorLocated(t *testing.T) {
	m, err := config.ParseManifests([]byte(`
kind: Repository
metadata: { name: a }
spec: { type: hosted, protocol: maven }
---
kind: Repository
metadata: { name: b }
spec: { type: virtual, protocol: oci }
`), "m.yaml")
	if err != nil {
		t.Fatal(err)
	}
	es := config.AsErrors(reconcile.ValidateSet(m, reconcile.ValidateOptions{}))
	if len(es) != 2 {
		t.Fatalf("got %d errors, want 2: %v", len(es), es)
	}
	for _, e := range es {
		if e.File != "m.yaml" || e.Line == 0 {
			t.Errorf("unlocated error: %#v", e)
		}
	}
}

// A token is a static Bearer value. An OCI registry wants Basic credentials at
// its token realm, where a static token has no meaning, so the pairing is
// refused when the repository is written (ADR 27).
func TestValidateRepository_OCIProxyRefusesTokenCredential(t *testing.T) {
	known := map[string]model.CredentialKind{"basic": model.CredentialBasic, "bearer": model.CredentialToken}
	tests := []struct {
		name     string
		protocol model.Protocol
		ref      string
		wantErr  []string
	}{
		{name: "OCIWithPasswordIsAccepted", protocol: model.ProtocolOCI, ref: "basic"},
		{name: "NPMWithTokenIsAccepted", protocol: model.ProtocolNPM, ref: "bearer"},
		{name: "PyPIWithTokenIsAccepted", protocol: model.ProtocolPyPI, ref: "bearer"},
		{name: "OCIWithTokenIsRefused", protocol: model.ProtocolOCI, ref: "bearer", wantErr: []string{`"bearer"`, "username/password"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := reconcile.RepositorySpec{
				Name: "p", Type: model.RepoTypeProxy, Protocol: tc.protocol,
				UpstreamURL: "https://registry.example.com", CredentialsRef: tc.ref,
			}
			err := reconcile.ValidateRepository(spec, known)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, reconcile.ErrCredentialKind) {
				t.Fatalf("want ErrCredentialKind, got %v", err)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestValidateSet_OCIProxyRefusesTokenCredential(t *testing.T) {
	m, err := config.ParseManifests([]byte(`
kind: Repository
metadata: { name: p }
spec:
  type: proxy
  protocol: oci
  upstream: { url: https://r.example.com, ttl: 1h, negativeTtl: 1m, credentials: bearer }
`), "m.yaml")
	if err != nil {
		t.Fatal(err)
	}
	err = reconcile.ValidateSet(m, reconcile.ValidateOptions{Credentials: map[string]model.CredentialKind{"bearer": model.CredentialToken}})
	if err == nil || !strings.Contains(err.Error(), "username/password") {
		t.Fatalf("want a username/password refusal, got %v", err)
	}
}

// SIN-116: a secret written into upstream.url would be stored, shown in the
// UI and printed by diff. Both spellings refuse it, and the error never echoes
// the URL.
func TestValidateSet_UpstreamURLWithCredentialsIsRefused(t *testing.T) {
	const secretURL = "https://alice:s3cret@pypi.mirror.invalid/simple"
	long := `
kind: Repository
metadata: { name: mirror }
spec:
  type: proxy
  protocol: pypi
  upstream: { url: ` + secretURL + ` }
`
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "config", "sindri.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const clean = "https://pypi.mirror.invalid/simple"
	if !strings.Contains(string(fixture), clean) {
		t.Fatalf("fixture no longer contains %s", clean)
	}
	short := strings.Replace(string(fixture), clean, secretURL, 1)

	tests := []struct {
		name string
		load func(t *testing.T) (*config.Manifests, map[string]model.CredentialKind)
	}{
		{"LongForm", func(t *testing.T) (*config.Manifests, map[string]model.CredentialKind) {
			m, err := config.ParseManifests([]byte(long), "m.yaml")
			if err != nil {
				t.Fatal(err)
			}
			return m, nil
		}},
		{"ShortForm", func(t *testing.T) (*config.Manifests, map[string]model.CredentialKind) {
			path := filepath.Join(t.TempDir(), "sindri.yaml")
			if err := os.WriteFile(path, []byte(short), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			return cfg.Desired, cfg.Credentials.Names()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, creds := tc.load(t)
			err := reconcile.ValidateSet(m, reconcile.ValidateOptions{Credentials: creds})
			if !errors.Is(err, reconcile.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			msg := err.Error()
			for _, leak := range []string{"s3cret", "alice", "@pypi"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error echoes %q: %s", leak, msg)
				}
			}
			if !strings.Contains(msg, "upstream.url") || !strings.Contains(msg, "credentials") {
				t.Errorf("error does not name the field and point at credentials: %s", msg)
			}
		})
	}
}

func TestValidateSet_NonHTTPUpstreamDoesNotEchoTheURL(t *testing.T) {
	m, perr := config.ParseManifests([]byte(`
kind: Repository
metadata: { name: mirror }
spec:
  type: proxy
  protocol: pypi
  upstream: { url: "ftp://bob:hunter2@host/simple" }
`), "m.yaml")
	if perr != nil {
		t.Fatal(perr)
	}
	err := reconcile.ValidateSet(m, reconcile.ValidateOptions{})
	if !errors.Is(err, reconcile.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoes the URL: %v", err)
	}
}
