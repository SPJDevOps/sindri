package remote

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNPMPackumentURL_EscapesTheScopeSlash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		upstream string
		pkg      string
		want     string
	}{
		{name: "Unscoped", upstream: "https://r.example", pkg: "foo", want: "https://r.example/foo"},
		{name: "Scoped", upstream: "https://r.example", pkg: "@acme/ui", want: "https://r.example/@acme%2fui"},
		{name: "TrailingSlash", upstream: "https://r.example/npm/", pkg: "@acme/ui", want: "https://r.example/npm/@acme%2fui"},
		{name: "Legacy", upstream: "https://r.example", pkg: "JSONStream", want: "https://r.example/JSONStream"},
		{name: "UnsafeCharactersEscaped", upstream: "https://r.example", pkg: "a?b", want: "https://r.example/a%3Fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := npmPackumentURL(tc.upstream, tc.pkg); got != tc.want {
				t.Fatalf("npmPackumentURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNPMTarballURL_OnlyTheUpstreamsHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		upstream string
		raw      string
		want     string
		wantErr  bool
	}{
		{name: "SameHost", upstream: "https://r.example", raw: "https://r.example/foo/-/foo-1.0.0.tgz", want: "https://r.example/foo/-/foo-1.0.0.tgz"},
		{name: "SchemeIsForcedToTheUpstreams", upstream: "https://r.example", raw: "http://r.example/foo/-/foo-1.0.0.tgz", want: "https://r.example/foo/-/foo-1.0.0.tgz"},
		{name: "ForcedDownToo", upstream: "http://r.example:8080", raw: "https://r.example:8080/f.tgz", want: "http://r.example:8080/f.tgz"},
		{name: "HostCaseIgnored", upstream: "https://r.example", raw: "https://R.Example/f.tgz", want: "https://r.example/f.tgz"},
		{name: "ExplicitDefaultPort", upstream: "https://r.example", raw: "https://r.example:443/f.tgz", want: "https://r.example/f.tgz"},
		{name: "OtherSchemesDefaultPort", upstream: "https://r.example", raw: "http://r.example:80/f.tgz", want: "https://r.example/f.tgz"},
		{name: "Relative", upstream: "https://r.example/npm", raw: "foo/-/foo-1.0.0.tgz", want: "https://r.example/npm/foo/-/foo-1.0.0.tgz"},
		{name: "RootRelative", upstream: "https://r.example/npm", raw: "/foo/-/foo-1.0.0.tgz", want: "https://r.example/foo/-/foo-1.0.0.tgz"},
		{name: "QueryKept", upstream: "https://r.example", raw: "https://r.example/f.tgz?sig=a", want: "https://r.example/f.tgz?sig=a"},
		{name: "UserinfoDropped", upstream: "https://r.example", raw: "https://me:pw@r.example/f.tgz", want: "https://r.example/f.tgz"},
		{name: "FragmentDropped", upstream: "https://r.example", raw: "https://r.example/f.tgz#x", want: "https://r.example/f.tgz"},
		{name: "OtherHost", upstream: "https://r.example", raw: "https://evil.example/f.tgz", wantErr: true},
		{name: "OtherPort", upstream: "https://r.example", raw: "https://r.example:8443/f.tgz", wantErr: true},
		{name: "LookalikeSuffix", upstream: "https://r.example", raw: "https://r.example.evil/f.tgz", wantErr: true},
		{name: "SubdomainIsAnotherHost", upstream: "https://r.example", raw: "https://cdn.r.example/f.tgz", wantErr: true},
		{name: "UserinfoSpoof", upstream: "https://r.example", raw: "https://r.example@evil.example/f.tgz", wantErr: true},
		{name: "ProtocolRelative", upstream: "https://r.example", raw: "//evil.example/f.tgz", wantErr: true},
		{name: "InternalAddress", upstream: "https://r.example", raw: "http://169.254.169.254/latest/f.tgz", wantErr: true},
		{name: "FileScheme", upstream: "https://r.example", raw: "file:///etc/f.tgz", wantErr: true},
		{name: "Opaque", upstream: "https://r.example", raw: "https:f.tgz", wantErr: true},
		{name: "Unparsable", upstream: "https://r.example", raw: "https://r.example\x7f/f.tgz", wantErr: true},
		{name: "Empty", upstream: "https://r.example", raw: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := npmTarballURL(tc.upstream, tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("npmTarballURL = %q, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("npmTarballURL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("npmTarballURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidNPMFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "Plain", in: "foo-1.0.0.tgz", want: true},
		{name: "Legacy", in: "JSONStream-1.3.5.tgz", want: true},
		{name: "Prerelease", in: "foo-1.0.0-rc.1.tgz", want: true},
		{name: "Build", in: "foo-1.0.0+abc.tgz", want: true},
		{name: "LegacyPunctuation", in: "it's(a)~name!-1.0.0.tgz", want: true},
		{name: "NotATarball", in: "foo-1.0.0.sh", want: false},
		{name: "OnlyTheSuffix", in: ".tgz", want: false},
		{name: "LeadingDot", in: ".foo-1.0.0.tgz", want: false},
		{name: "Slash", in: "a/foo.tgz", want: false},
		{name: "Backslash", in: `a\foo.tgz`, want: false},
		{name: "Percent", in: "a%2ffoo.tgz", want: false},
		{name: "Quote", in: `a"foo.tgz`, want: false},
		{name: "Space", in: "a foo.tgz", want: false},
		{name: "NUL", in: "a\x00foo.tgz", want: false},
		{name: "Newline", in: "a\nfoo.tgz", want: false},
		{name: "NonASCII", in: "Koo-1.0.0.tgz", want: false},
		{name: "Query", in: "foo.tgz?x", want: false},
		{name: "TooLong", in: strings.Repeat("a", 600) + ".tgz", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidNPMFilename(tc.in); got != tc.want {
				t.Fatalf("ValidNPMFilename(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseNPMPackument_KeepsEveryFieldAndOmitsWhatCannotBeServed(t *testing.T) {
	t.Parallel()
	body := `{
		"name": "foo",
		"modified": "2026-01-01T00:00:00.000Z",
		"dist-tags": {"latest": "1.0.0", "bad": "9.9.9", "gone": "3.0.0"},
		"versions": {
			"1.0.0": {"name": "foo", "version": "1.0.0", "engines": {"node": ">=18"},
				"dist": {"tarball": "https://r.example/foo/-/foo-1.0.0.tgz", "integrity": "sha512-abc", "shasum": "def",
					"signatures": [{"keyid": "k", "sig": "s"}]}},
			"2.0.0": {"dist": {"tarball": "https://evil.example/foo-2.0.0.tgz"}},
			"9.9.9": {"dist": {"tarball": "https://r.example/x/-/..%2f..%2fetc.tgz"}},
			"8.0.0": {"dist": {"tarball": "https://r.example/foo/-/payload.sh"}},
			"7.0.0": {"dist": {}},
			"6.0.0": {"dist": "not an object"},
			"5.0.0": "not an object",
			"../4.0.0": {"dist": {"tarball": "https://r.example/foo/-/foo-4.0.0.tgz"}},
			"3.0.0": {"DIST": {"tarball": "https://r.example/foo/-/foo-3.0.0.tgz"}}
		}
	}`
	doc, err := parseNPMPackument("foo", []byte(body))
	if err != nil {
		t.Fatalf("parseNPMPackument: %v", err)
	}
	got := make([]string, 0, len(doc.Versions))
	for v := range doc.Versions {
		got = append(got, v)
	}
	if len(got) != 2 {
		t.Fatalf("versions = %v, want 1.0.0 and 2.0.0", got)
	}
	v1 := doc.Versions["1.0.0"]
	if v1.Filename != "foo-1.0.0.tgz" || v1.Integrity != "sha512-abc" || v1.Shasum != "def" {
		t.Fatalf("1.0.0 = %+v", v1)
	}
	if string(v1.Dist["signatures"]) != `[{"keyid": "k", "sig": "s"}]` {
		t.Fatalf("signatures = %s", v1.Dist["signatures"])
	}
	if string(v1.Fields["engines"]) != `{"node": ">=18"}` {
		t.Fatalf("engines = %s", v1.Fields["engines"])
	}
	if doc.Versions["2.0.0"].Filename != "foo-2.0.0.tgz" || doc.Versions["2.0.0"].Integrity != "" {
		t.Fatalf("2.0.0 = %+v, a cross-host or hashless version stays listed", doc.Versions["2.0.0"])
	}
	if string(doc.Fields["modified"]) != `"2026-01-01T00:00:00.000Z"` {
		t.Fatalf("modified = %s", doc.Fields["modified"])
	}
	if diff := cmp.Diff(map[string]string{"latest": "1.0.0"}, doc.DistTags); diff != "" {
		t.Fatalf("a tag naming an omitted version must go with it (-want +got):\n%s", diff)
	}
}

func TestParseNPMPackument_RefusesADocumentItCannotTrust(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "NotJSON", body: `<html>`},
		{name: "NotAnObject", body: `[]`},
		{name: "NoName", body: `{"versions": {}}`},
		{name: "OtherName", body: `{"name": "bar", "versions": {}}`},
		{name: "OtherCase", body: `{"name": "FOO", "versions": {}}`},
		{name: "NameInAnotherCase", body: `{"NAME": "foo", "versions": {}}`},
		{name: "VersionsNotAnObject", body: `{"name": "foo", "versions": []}`},
		{name: "DistTagsNotStrings", body: `{"name": "foo", "dist-tags": {"latest": 1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseNPMPackument("foo", []byte(tc.body)); err == nil {
				t.Fatal("parsed, want an error")
			}
		})
	}
}

func TestNPMPackument_VersionWithFilename(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{}`)
	doc := NPMPackument{Versions: map[string]NPMVersion{
		"1.0.0": {Version: "1.0.0", Filename: "odd-name.tgz", Fields: map[string]json.RawMessage{"a": raw}},
		"2.0.0": {Version: "2.0.0", Filename: "twice.tgz"},
		"3.0.0": {Version: "3.0.0", Filename: "twice.tgz"},
	}}
	// The filename is looked up, never parsed: this one names no version at all.
	if v, err := doc.versionWithFilename("odd-name.tgz"); err != nil || v.Version != "1.0.0" {
		t.Fatalf("versionWithFilename = %+v, %v", v, err)
	}
	if _, err := doc.versionWithFilename("absent.tgz"); !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("absent = %v, want ErrUpstreamNotFound", err)
	}
	// Two versions claiming one file could only choose between integrities by
	// map order, so neither is served.
	if _, err := doc.versionWithFilename("twice.tgz"); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("ambiguous = %v, want ErrUpstreamUnavailable", err)
	}
}
