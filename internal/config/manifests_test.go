package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/model"
)

// ignoreSource drops error locations from comparisons: where a document was
// written is not part of the state it describes.
var ignoreSource = cmp.Options{
	cmpopts.IgnoreFields(config.Repository{}, "Source"),
	cmpopts.IgnoreFields(config.Lifecycle{}, "Source"),
	cmpopts.IgnoreFields(config.OrgPolicy{}, "Source"),
}

// The desugaring guarantee of ADR 7 for the desired-state sections: the
// short form in one sindri.yaml and the long form in a manifest directory load
// into identical trees. If this fails, the two spellings have become two
// parsers. Do not relax it; fix whichever side drifted.
func TestManifests_ShortAndLongFormsAreIdentical(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "config", "sindri.yaml"))
	if err != nil {
		t.Fatalf("loading the short form: %v", err)
	}
	if cfg.Desired == nil {
		t.Fatal("the short form produced no desired state")
	}
	long, err := config.LoadManifests(filepath.Join("..", "..", "testdata", "manifests"))
	if err != nil {
		t.Fatalf("loading the long form: %v", err)
	}
	if diff := cmp.Diff(long, cfg.Desired, ignoreSource); diff != "" {
		t.Fatalf("short and long forms differ (-long +short):\n%s", diff)
	}
}

// The expanded form is itself valid long form, and reads back unchanged. That
// is what makes `config show --expanded` a way to learn the explicit form
// rather than a lossy summary.
func TestMarshalManifests_RoundTrips(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "config", "sindri.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.MarshalManifests(cfg.Desired)
	if err != nil {
		t.Fatal(err)
	}
	back, err := config.ParseManifests(raw, "expanded.yaml")
	if err != nil {
		t.Fatalf("expanded output does not load:\n%s\n%v", raw, err)
	}
	if diff := cmp.Diff(cfg.Desired, back, ignoreSource); diff != "" {
		t.Fatalf("round trip changed the tree (-before +after):\n%s", diff)
	}
}

func TestDesugar_InfersTypesAndProtocols(t *testing.T) {
	cfg := mustParse(t, `
repos:
  internal: { protocol: pypi }
  pypi:     { upstream: pypi }
  python:   { members: [internal, pypi] }
`)
	got := map[string]config.RepositorySpec{}
	for _, r := range cfg.Desired.Repositories {
		got[r.Name] = r.Spec
	}
	want := map[string]config.RepositorySpec{
		"internal": {Type: model.RepoTypeHosted, Protocol: model.ProtocolPyPI},
		"pypi": {Type: model.RepoTypeProxy, Protocol: model.ProtocolPyPI, Upstream: &config.Upstream{
			URL: "https://pypi.org/simple", TTL: config.Duration(time.Hour), NegativeTTL: config.Duration(5 * time.Minute),
		}},
		"python": {Type: model.RepoTypeVirtual, Protocol: model.ProtocolPyPI, Members: []config.MemberRef{
			{Ref: "hosted/internal"}, {Ref: "proxy/pypi"},
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("desugared specs differ (-want +got):\n%s", diff)
	}
}

// Precedence for a proxy's upstream, lowest first: preset, defaults.proxy,
// then the repository's own object.
func TestDesugar_UpstreamPrecedence(t *testing.T) {
	cfg := mustParse(t, `
credentials:
  mirror: { username: u, password: p }
defaults:
  proxy: { ttl: 10m, credentials: mirror }
repos:
  a: { upstream: pypi }
  b: { upstream: { preset: pypi, ttl: 2m } }
  c: { protocol: oci, upstream: { url: https://registry.example.com, negativeTtl: 1m } }
`)
	got := map[string]config.Upstream{}
	for _, r := range cfg.Desired.Repositories {
		got[r.Name] = *r.Spec.Upstream
	}
	want := map[string]config.Upstream{
		"a": {URL: "https://pypi.org/simple", TTL: config.Duration(10 * time.Minute), NegativeTTL: config.Duration(5 * time.Minute), Credentials: "mirror"},
		"b": {URL: "https://pypi.org/simple", TTL: config.Duration(2 * time.Minute), NegativeTTL: config.Duration(5 * time.Minute), Credentials: "mirror"},
		"c": {URL: "https://registry.example.com", TTL: config.Duration(10 * time.Minute), NegativeTTL: config.Duration(time.Minute), Credentials: "mirror"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("upstreams differ (-want +got):\n%s", diff)
	}
}

func TestDesugar_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string // substrings the operator needs to see, beyond the file name
	}{
		{
			name:  "MembersAndUpstream",
			input: "repos:\n  a: { protocol: pypi }\n  bad:\n    members: [a]\n    upstream: pypi\n",
			want:  []string{"sindri.yaml:5:", "repos.bad", "both members (line 4) and upstream (line 5)"},
		},
		{
			name: "VirtualMembersDisagreeOnProtocol",
			input: "repos:\n  py: { protocol: pypi }\n  img: { protocol: oci }\n" +
				"  mixed:\n    members: [py, img]\n",
			want: []string{"sindri.yaml:5:", "repos.mixed.members", "py is pypi, img is oci"},
		},
		{
			name:  "ExplicitTypeContradictsInference",
			input: "repos:\n  a: { type: hosted, upstream: pypi }\n",
			want:  []string{"repos.a.type", "upstream makes it proxy"},
		},
		{
			name:  "HostedWithoutProtocol",
			input: "repos:\n  a: {}\n",
			want:  []string{"repos.a.protocol", "needs a protocol"},
		},
		{
			name:  "UnknownPreset",
			input: "repos:\n  a: { upstream: pypy }\n",
			want:  []string{"repos.a.upstream", `"pypy" is not a preset`},
		},
		{
			name:  "PresetForUnservedProtocol",
			input: "repos:\n  a: { upstream: npm }\n",
			want:  []string{"repos.a.upstream", "this build does not serve"},
		},
		{
			name:  "UnknownMember",
			input: "repos:\n  v: { members: [ghost] }\n",
			want:  []string{"repos.v.members[0]", `"ghost" is not declared`},
		},
		{
			name:  "BareNumberTTL",
			input: "repos:\n  p: { upstream: { preset: pypi, ttl: 3600 } }\n",
			want:  []string{"repos.p.upstream.ttl", "needs a unit"},
		},
		{
			name:  "UnknownRepoKey",
			input: "repos:\n  a: { protocol: pypi, mebers: [x] }\n",
			want:  []string{"repos.a", `unknown field "mebers"`},
		},
		{
			name:  "GateOnUndeclaredStage",
			input: "lifecycle:\n  stages: [dev]\n  gates:\n    prod: [sbom]\n",
			want:  []string{"sindri.yaml:4:", "lifecycle.gates.prod", "does not declare"},
		},
		{
			name:  "HostedDefaultsTakeNoTTL",
			input: "defaults:\n  hosted: { ttl: 1h }\n",
			want:  []string{"defaults.hosted", `unknown field "ttl"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.input), "sindri.yaml")
			if err == nil {
				t.Fatalf("Parse accepted:\n%s", tc.input)
			}
			for _, w := range append([]string{"sindri.yaml"}, tc.want...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

// Every mistake is reported in one pass, so a pull request is fixed in one
// round trip.
func TestDesugar_ReportsEveryError(t *testing.T) {
	_, err := config.Parse([]byte("repos:\n  a: {}\n  b: { upstream: nope }\n"), "sindri.yaml")
	if got := len(config.AsErrors(err)); got != 2 {
		t.Fatalf("got %d errors, want 2: %v", got, err)
	}
}

func TestLoadManifests_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"MissingKind", "metadata: { name: a }\n", []string{"needs a kind"}},
		{"UnknownKind", "kind: Repo\n", []string{`"Repo" is not a kind`}},
		{"MissingType", "kind: Repository\nmetadata: { name: a }\nspec: { protocol: oci }\n", []string{"Repository[a].spec.type", "is required"}},
		{"UnknownSpecField", "kind: Repository\nmetadata: { name: a }\nspec: { type: hosted, protocol: oci, upstrem: x }\n", []string{`unknown field "upstrem"`}},
		{"TwoLifecycles", "kind: Lifecycle\nspec: { stages: [] }\n---\nkind: Lifecycle\nspec: { stages: [] }\n", []string{"declared twice"}},
		{"BadMode", "kind: OrgPolicy\nspec: { mode: anarchy }\n", []string{"OrgPolicy.spec.mode"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.ParseManifests([]byte(tc.input), "m.yaml")
			if err == nil {
				t.Fatalf("accepted:\n%s", tc.input)
			}
			for _, w := range append([]string{"m.yaml"}, tc.want...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestLoadManifests_NamespacedNames(t *testing.T) {
	m, err := config.ParseManifests([]byte(`
kind: Repository
metadata: { name: python, namespace: payments }
spec: { type: virtual, protocol: pypi, members: [{ ref: hosted/internal }] }
`), "m.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Repositories[0].FullName(); got != "payments/python" {
		t.Fatalf("FullName = %q, want payments/python", got)
	}
}

// Anchors and merge keys are the sanctioned alternative to `extends:`.
func TestDesugar_AnchorsStandInForExtends(t *testing.T) {
	cfg := mustParse(t, `
repos:
  a: &py { protocol: pypi, read: [team:x] }
  b: { <<: *py, write: [team:y] }
`)
	b := cfg.Desired.Repositories[1]
	if b.Spec.Protocol != model.ProtocolPyPI || len(b.Spec.Access.Read) != 1 || b.Spec.Access.Write[0] != "team:y" {
		t.Fatalf("merge key not applied: %+v", b.Spec)
	}
}

func TestShow_RedactsSecrets(t *testing.T) {
	t.Setenv("M7_SECRET", "hunter2-upstream")
	cfg := mustParse(t, `
database: postgres://sindri:dbpass-123@db:5432/sindri
auth:
  static_token: bootstrap-secret-xyz
  oidc: { issuer: https://idp.example.com, clientId: sindri, clientSecret: oidc-secret-abc }
credentials:
  mirror:
    username: robot
    password: ${M7_SECRET}
`)
	raw, err := config.MarshalServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, secret := range []string{"hunter2-upstream", "dbpass-123", "bootstrap-secret-xyz", "oidc-secret-abc", "robot"} {
		if strings.Contains(out, secret) {
			t.Errorf("config show printed %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "mirror") {
		t.Errorf("config show hid the credential's name, which is not a secret:\n%s", out)
	}
	if cfg.Credentials["mirror"].Password != "hunter2-upstream" {
		t.Error("redacting for display changed the loaded configuration")
	}
}

func TestLooksLikeManifests(t *testing.T) {
	if !config.LooksLikeManifests([]byte("# c\nkind: Repository\n")) {
		t.Error("an enveloped document was not recognised")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "config", "sindri.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if config.LooksLikeManifests(raw) {
		t.Error("sindri.yaml was taken for manifests")
	}
}

func TestAsErrors_KeepsLocations(t *testing.T) {
	_, err := config.Parse([]byte("repos:\n  a: {}\n"), "f.yaml")
	es := config.AsErrors(err)
	var fe *config.FieldError
	if len(es) != 1 || !errors.As(es[0], &fe) || fe.File != "f.yaml" || fe.Line != 2 {
		t.Fatalf("location lost: %#v", es)
	}
}

func mustParse(t *testing.T, input string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(input), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}
