package reconcile_test

import (
	"errors"
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
	known := map[string]struct{}{"mirror": {}}
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
			opts:  reconcile.ValidateOptions{Credentials: map[string]struct{}{}},
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
spec: { type: hosted, protocol: npm }
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
