package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestValidateRepoName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "internal"},
		{name: "nested", in: "team/internal"},
		{name: "punctuation", in: "team.a_b-c/internal"},
		{name: "illegal character", in: "team internal", wantErr: true},
		{name: "leading slash", in: "/internal", wantErr: true},
		{name: "trailing slash", in: "internal/", wantErr: true},
		{name: "empty segment", in: "team//internal", wantErr: true},
		// A repository name that contains an adapter's path marker makes the
		// route ambiguous: /pypi/team/packages/simple/foo/ would name either
		// the simple index of "team/packages" or a download from "team".
		{name: "reserved pypi simple", in: "team/simple", wantErr: true},
		{name: "reserved pypi packages", in: "team/packages/x", wantErr: true},
		{name: "reserved pypi upload", in: "upload", wantErr: true},
		{name: "reserved oci blobs", in: "team/blobs", wantErr: true},
		{name: "reserved oci manifests", in: "manifests/team", wantErr: true},
		{name: "reserved oci tags", in: "team/tags/x", wantErr: true},
		{name: "reserved oci referrers", in: "team/referrers/x", wantErr: true},
		{name: "reserved regardless of case", in: "team/Packages", wantErr: true},
		{name: "reserved word as substring is fine", in: "team/packages-internal"},
		// The UI separates a repository name from the page within it with
		// /-/, so a lone dash segment would make /ui/r/team/-/... ambiguous.
		{name: "reserved ui separator", in: "team/-/x", wantErr: true},
		{name: "dash inside a segment is fine", in: "team/-x-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRepoName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateRepoName(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("ValidateRepoName(%q) error does not wrap ErrInvalidInput: %v", tc.in, err)
			}
		})
	}
}

func TestNormalizePackageName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "pep503 fold", in: "Foo.Bar_Baz", want: "foo-bar-baz"},
		{name: "runs collapse", in: "foo___bar", want: "foo-bar"},
		{name: "separators only", in: "___", want: ""},
		{name: "slashes are dropped", in: "a/b", want: "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizePackageName(tc.in); got != tc.want {
				t.Fatalf("normalizePackageName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeName_NpmNamesDoNotCollide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		protocol model.Protocol
		in       string
		want     string
	}{
		{name: "scoped name keeps scope and slash", protocol: model.ProtocolNPM, in: "@acme/ui", want: "@acme/ui"},
		{name: "unscoped name with no separator", protocol: model.ProtocolNPM, in: "acmeui", want: "acmeui"},
		{name: "dot is kept", protocol: model.ProtocolNPM, in: "foo.bar", want: "foo.bar"},
		{name: "underscore is kept", protocol: model.ProtocolNPM, in: "foo_bar", want: "foo_bar"},
		{name: "case is kept", protocol: model.ProtocolNPM, in: "JSONStream", want: "JSONStream"},
		{name: "pypi still folds", protocol: model.ProtocolPyPI, in: "Foo_Bar.baz", want: "foo-bar-baz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeName(tc.protocol, tc.in); got != tc.want {
				t.Fatalf("NormalizeName(%q, %q) = %q, want %q", tc.protocol, tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateCreateParams_ProxyUpstreamWithCredentialsIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "PlainURLIsAccepted", url: "https://registry.example.com"},
		{name: "UserAndPasswordAreRefused", url: "https://alice:s3cret@registry.example.com", wantErr: true},
		{name: "TokenAsUserIsRefused", url: "https://ghp_abc123@registry.example.com", wantErr: true},
		{name: "EmptyUserWithPasswordIsRefused", url: "https://:s3cret@registry.example.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCreateParams(CreateRepositoryParams{
				Name: "p", Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI, UpstreamURL: tc.url,
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			for _, leak := range []string{"s3cret", "alice", "ghp_abc123", tc.url} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error echoes %q: %v", leak, err)
				}
			}
			if !strings.Contains(err.Error(), "credentials") {
				t.Errorf("error does not point at credentials: %v", err)
			}
		})
	}
}

func TestCurrentVersion_PrefersTheLatestTagThenTheHighestVersion(t *testing.T) {
	tests := []struct {
		name     string
		protocol model.Protocol
		latest   string
		versions []string
		want     string
	}{
		{"NPMLatestTagWins", model.ProtocolNPM, "1.9.0", []string{"1.10.0", "1.9.0", "2.0.0-rc.1"}, "1.9.0"},
		{"NPMWithoutATagTakesTheHighestSemver", model.ProtocolNPM, "", []string{"1.9.0", "1.10.0"}, "1.10.0"},
		{"NPMTagNamingAMissingVersionIsIgnored", model.ProtocolNPM, "3.0.0", []string{"1.9.0", "1.10.0"}, "1.10.0"},
		{"PyPITakesTheHighestPEP440", model.ProtocolPyPI, "", []string{"1.0rc1", "1.0", "1.0.post1"}, "1.0.post1"},
		{"NoVersions", model.ProtocolNPM, "1.0.0", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CurrentVersion(tc.protocol, tc.latest, tc.versions); got != tc.want {
				t.Errorf("CurrentVersion = %q, want %q", got, tc.want)
			}
		})
	}
}
