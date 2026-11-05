package npm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

func TestTarballURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                       string
		publicURL, repo, pkg, file string
		want                       string
	}{
		{
			name: "scoped keeps its slash", publicURL: "https://s.example", repo: "team/internal", pkg: "@acme/ui", file: "ui-1.0.0.tgz",
			want: "https://s.example/npm/team/internal/@acme/ui/-/ui-1.0.0.tgz",
		},
		{
			name: "unscoped", publicURL: "https://s.example", repo: "r", pkg: "foo.bar", file: "foo.bar-1.0.0.tgz",
			want: "https://s.example/npm/r/foo.bar/-/foo.bar-1.0.0.tgz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tarballURL(tc.publicURL, tc.repo, tc.pkg, tc.file); got != tc.want {
				t.Fatalf("tarballURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveDistTags(t *testing.T) {
	t.Parallel()
	versions := func(vs ...string) []registry.NPMVersion {
		out := make([]registry.NPMVersion, 0, len(vs))
		for _, v := range vs {
			out = append(out, registry.NPMVersion{Version: v})
		}
		return out
	}
	cases := []struct {
		name     string
		versions []registry.NPMVersion
		tags     map[string]string
		want     map[string]string
	}{
		{
			name:     "a stored latest wins over a higher version",
			versions: versions("1.0.0", "2.0.0"),
			tags:     map[string]string{"latest": "1.0.0"},
			want:     map[string]string{"latest": "1.0.0"},
		},
		{
			name:     "versions compare numerically, not as text",
			versions: versions("1.10.0", "1.9.0"),
			tags:     map[string]string{"next": "1.9.0"},
			want:     map[string]string{"latest": "1.10.0", "next": "1.9.0"},
		},
		{
			name:     "a prerelease loses to a lower release",
			versions: versions("1.0.0", "2.0.0-rc.1"),
			want:     map[string]string{"latest": "1.0.0"},
		},
		{
			name:     "only prereleases gives the highest of them",
			versions: versions("2.0.0-beta.2", "2.0.0-beta.10", "1.0.0-alpha"),
			want:     map[string]string{"latest": "2.0.0-beta.10"},
		},
		{
			name: "no versions gives no latest",
			want: map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := effectiveDistTags(tc.versions, tc.tags)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("effectiveDistTags (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEffectiveDistTags_LeavesItsInputAlone(t *testing.T) {
	t.Parallel()
	tags := map[string]string{"next": "1.0.0"}
	_ = effectiveDistTags([]registry.NPMVersion{{Version: "1.0.0"}}, tags)
	if diff := cmp.Diff(map[string]string{"next": "1.0.0"}, tags); diff != "" {
		t.Fatalf("input changed (-want +got):\n%s", diff)
	}
}

func TestIsTarballFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pkg  string
		file string
		want bool
	}{
		{name: "unscoped", pkg: "foo", file: "foo-1.0.0.tgz", want: true},
		{name: "scoped drops its scope", pkg: "@acme/ui", file: "ui-1.0.0.tgz", want: true},
		{name: "prerelease", pkg: "foo", file: "foo-1.0.0-rc.1.tgz", want: true},
		{name: "scoped spelled with its scope", pkg: "@acme/ui", file: "@acme/ui-1.0.0.tgz"},
		{name: "another package", pkg: "foo", file: "bar-1.0.0.tgz"},
		{name: "name with no version", pkg: "foo", file: "foo.tgz"},
		{name: "short version", pkg: "foo", file: "foo-1.0.tgz"},
		{name: "NUL", pkg: "foo", file: "foo-1.0.0\x00.tgz"},
		{name: "invalid UTF-8", pkg: "foo", file: "foo-1.0.0\xff.tgz"},
		{name: "build metadata", pkg: "foo", file: "foo-1.0.0+a.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTarballFilename(tc.pkg, tc.file); got != tc.want {
				t.Fatalf("isTarballFilename(%q, %q) = %v, want %v", tc.pkg, tc.file, got, tc.want)
			}
		})
	}
}

// timedPackage is a hosted package whose versions carry the given publish times;
// a zero time is a row with no recorded time.
func timedPackage(times map[string]time.Time) registry.NPMPackage {
	pkg := registry.NPMPackage{Name: "foo", DistTags: map[string]string{}}
	for version, at := range times {
		pkg.Versions = append(pkg.Versions, registry.NPMVersion{
			Version:     version,
			PublishedAt: at,
			File: registry.PackageFile{
				Filename: "foo-" + version + ".tgz",
				Metadata: json.RawMessage(`{"manifest":{"name":"foo"},"integrity":"sha512-x","shasum":"y"}`),
			},
		})
	}
	return pkg
}

func derivedTime(t *testing.T, pkg registry.NPMPackage) map[string]string {
	t.Helper()
	h := &handler{publicURL: "https://s.example"}
	doc, err := h.derivePackument("r", pkg)
	if err != nil {
		t.Fatalf("derivePackument: %v", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	var out struct {
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return out.Time
}

func TestPackument_HostedReportsRealPublishTimes(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	second := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("CET", 3600))
	got := derivedTime(t, timedPackage(map[string]time.Time{"1.0.0": first, "1.1.0": second}))
	want := map[string]string{
		"1.0.0":    "2026-01-02T03:04:05.678Z",
		"1.1.0":    "2026-03-04T04:06:07.000Z",
		"created":  "2026-01-02T03:04:05.678Z",
		"modified": "2026-03-04T04:06:07.000Z",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("time (-want +got):\n%s", diff)
	}
}

func TestPackument_VersionWithNoTimeIsLeftOut(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t.Run("a NULL row is absent and the rest are reported", func(t *testing.T) {
		t.Parallel()
		got := derivedTime(t, timedPackage(map[string]time.Time{"1.0.0": {}, "1.1.0": at}))
		want := map[string]string{"1.1.0": "2026-01-02T03:04:05.000Z", "created": "2026-01-02T03:04:05.000Z", "modified": "2026-01-02T03:04:05.000Z"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("time (-want +got):\n%s", diff)
		}
	})
	t.Run("no real time anywhere omits the whole map", func(t *testing.T) {
		t.Parallel()
		if got := derivedTime(t, timedPackage(map[string]time.Time{"1.0.0": {}})); got != nil {
			t.Fatalf("time = %v, want none", got)
		}
	})
}

func TestRewriteUpstream_DropsAttachmentsAndResolved(t *testing.T) {
	t.Parallel()
	up := remote.NPMPackument{
		Name: "foo",
		Fields: map[string]json.RawMessage{
			"name":         json.RawMessage(`"foo"`),
			"_attachments": json.RawMessage(`{"foo-1.0.0.tgz":{"data":"AAAA"}}`),
			"_rev":         json.RawMessage(`"3-abc"`),
		},
		DistTags: map[string]string{"latest": "1.0.0"},
		Versions: map[string]remote.NPMVersion{"1.0.0": {
			Fields: map[string]json.RawMessage{
				"version":   json.RawMessage(`"1.0.0"`),
				"_resolved": json.RawMessage(`"https://registry.example/foo/-/foo-1.0.0.tgz"`),
				"_id":       json.RawMessage(`"foo@1.0.0"`),
			},
			Dist:     map[string]json.RawMessage{"shasum": json.RawMessage(`"y"`)},
			Filename: "foo-1.0.0.tgz",
		}},
	}
	h := &handler{publicURL: "https://s.example"}
	doc, err := h.rewriteUpstream("r", up)
	if err != nil {
		t.Fatalf("rewriteUpstream: %v", err)
	}
	if _, ok := doc["_attachments"]; ok {
		t.Error("top-level _attachments survived: it can carry upstream's tarball bytes")
	}
	if _, ok := doc["_rev"]; !ok {
		t.Error("an unrelated underscore field was dropped; only _attachments goes")
	}
	var versions map[string]map[string]json.RawMessage
	if err := json.Unmarshal(doc["versions"], &versions); err != nil {
		t.Fatalf("decoding versions: %v", err)
	}
	if _, ok := versions["1.0.0"]["_resolved"]; ok {
		t.Error("_resolved survived: it names upstream's tarball URL, behind Sindri's auth")
	}
	if _, ok := versions["1.0.0"]["_id"]; !ok {
		t.Error("an unrelated underscore field of a version was dropped; only _resolved goes")
	}
	if up.Fields["_attachments"] == nil || up.Versions["1.0.0"].Fields["_resolved"] == nil {
		t.Error("rewriteUpstream changed its input")
	}
}
