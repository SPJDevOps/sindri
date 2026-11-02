package npm

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/registry"
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
