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
		{name: "RootRelative", upstream: "https://r.example", raw: "/foo/-/foo-1.0.0.tgz", want: "https://r.example/foo/-/foo-1.0.0.tgz"},
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

// The proxy's credentials go with these requests, and on a shared host the path
// is what separates one tenant from another.
func TestNPMTarballURL_StaysUnderTheUpstreamPath(t *testing.T) {
	t.Parallel()
	const host = "https://r.example"
	cases := []struct {
		name     string
		upstream string
		raw      string
		want     string
		wantErr  bool
	}{
		{name: "EmptyBaseAdmitsAnyPath", upstream: host, raw: host + "/foo/-/foo-1.0.0.tgz", want: host + "/foo/-/foo-1.0.0.tgz"},
		{name: "EmptyBaseWithTrailingSlash", upstream: host + "/", raw: host + "/foo/-/foo-1.0.0.tgz", want: host + "/foo/-/foo-1.0.0.tgz"},
		{name: "UnderTheBase", upstream: host + "/npm-a", raw: host + "/npm-a/foo/-/foo-1.0.0.tgz", want: host + "/npm-a/foo/-/foo-1.0.0.tgz"},
		{name: "UnderTheBaseWithTrailingSlash", upstream: host + "/npm-a/", raw: host + "/npm-a/f.tgz", want: host + "/npm-a/f.tgz"},
		{name: "UnderANestedBase", upstream: host + "/repo/npm-a", raw: host + "/repo/npm-a/f.tgz", want: host + "/repo/npm-a/f.tgz"},
		{name: "RelativeStaysUnder", upstream: host + "/npm-a", raw: "foo/-/f.tgz", want: host + "/npm-a/foo/-/f.tgz"},
		{name: "OrdinaryEscapeKept", upstream: host + "/npm-a", raw: host + "/npm-a/a%20b/f.tgz", want: host + "/npm-a/a%20b/f.tgz"},
		{name: "DotsInsideASegmentAreNotDotSegments", upstream: host + "/npm-a", raw: host + "/npm-a/foo..bar/...tgz", want: host + "/npm-a/foo..bar/...tgz"},

		{name: "AnotherPath", upstream: host + "/npm-a", raw: host + "/npm-b/f.tgz", wantErr: true},
		{name: "SegmentPrefixIsNotUnder", upstream: host + "/npm-a", raw: host + "/npm-ab/f.tgz", wantErr: true},
		{name: "ParentOfTheBase", upstream: host + "/repo/npm-a", raw: host + "/repo/f.tgz", wantErr: true},
		{name: "RootRelativeLeavesTheBase", upstream: host + "/npm-a", raw: "/npm-b/f.tgz", wantErr: true},
		{name: "TheBaseItself", upstream: host + "/npm-a", raw: host + "/npm-a", wantErr: true},
		{name: "TheBaseDirectory", upstream: host + "/npm-a", raw: host + "/npm-a/", wantErr: true},
		{name: "BaseCaseMatters", upstream: host + "/npm-a", raw: host + "/NPM-A/f.tgz", wantErr: true},

		{name: "DotDot", upstream: host + "/npm-a", raw: host + "/npm-a/../npm-b/f.tgz", wantErr: true},
		{name: "DotDotThatStaysUnder", upstream: host + "/npm-a", raw: host + "/npm-a/x/../f.tgz", wantErr: true},
		{name: "Dot", upstream: host + "/npm-a", raw: host + "/npm-a/./f.tgz", wantErr: true},
		{name: "RelativeDotDot", upstream: host + "/npm-a", raw: "../npm-b/f.tgz", wantErr: true},
		{name: "DotDotOnAnEmptyBase", upstream: host, raw: host + "/a/../f.tgz", wantErr: true},
		{name: "EncodedDotDot", upstream: host + "/npm-a", raw: host + "/npm-a/%2e%2e/npm-b/f.tgz", wantErr: true},
		{name: "EncodedDotDotUppercase", upstream: host + "/npm-a", raw: host + "/npm-a/%2E%2E/npm-b/f.tgz", wantErr: true},
		{name: "HalfEncodedDotDot", upstream: host + "/npm-a", raw: host + "/npm-a/.%2e/npm-b/f.tgz", wantErr: true},
		{name: "EncodedDot", upstream: host + "/npm-a", raw: host + "/npm-a/%2e/f.tgz", wantErr: true},
		{name: "DoubleEncodedDotDot", upstream: host + "/npm-a", raw: host + "/npm-a/%252e%252e/npm-b/f.tgz", wantErr: true},
		{name: "EncodedSlash", upstream: host + "/npm-a", raw: host + "/npm-a/x%2f..%2f..%2fnpm-b/f.tgz", wantErr: true},
		{name: "EncodedSlashAlone", upstream: host + "/npm-a", raw: host + "/npm-a/x%2Ff.tgz", wantErr: true},
		{name: "DoubleEncodedSlash", upstream: host + "/npm-a", raw: host + "/npm-a/x%252ff.tgz", wantErr: true},
		{name: "EncodedSlashOnAnEmptyBase", upstream: host, raw: host + "/a%2fb.tgz", wantErr: true},
		{name: "Backslash", upstream: host + "/npm-a", raw: host + `/npm-a/..\npm-b/f.tgz`, wantErr: true},
		{name: "EncodedBackslash", upstream: host + "/npm-a", raw: host + "/npm-a/..%5cnpm-b/f.tgz", wantErr: true},
		{name: "DotDotWithPathParameter", upstream: host + "/npm-a", raw: host + "/npm-a/..;/npm-b/f.tgz", wantErr: true},
		{name: "EmptySegment", upstream: host + "/npm-a", raw: host + "/npm-a//f.tgz", wantErr: true},
		{name: "EncodedNUL", upstream: host + "/npm-a", raw: host + "/npm-a/f%00.tgz", wantErr: true},
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
