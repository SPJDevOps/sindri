package ui

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/web"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// The Content-Security-Policy allows no inline script, style, or event
// handler. Anything of the kind in a template would silently not work in a
// browser, or would tempt someone to loosen the policy; either way it is
// caught here, before review.
func TestTemplates_NoInlineScriptOrStyleOrHandlers(t *testing.T) {
	forbidden := []struct {
		what string
		re   *regexp.Regexp
	}{
		{"inline script", regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`)},
		{"script without src", regexp.MustCompile(`<script(?:(?:\s(?:defer|async|type="[^"]*"))*)\s*>`)},
		{"style element", regexp.MustCompile(`<style[\s>]`)},
		{"style attribute", regexp.MustCompile(`\sstyle=`)},
		{"event handler attribute", regexp.MustCompile(`\son[a-z]+=`)},
		{"htmx inline handler", regexp.MustCompile(`\shx-on`)},
		{"javascript: URL", regexp.MustCompile(`(?i)javascript:`)},
		{"third-party origin", regexp.MustCompile(`(?:src|href)="(?:https?:)?//`)},
	}
	err := fs.WalkDir(web.Templates(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web.Templates(), path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if loc := f.re.FindIndex(b); loc != nil {
				line := bytes.Count(b[:loc[0]], []byte("\n")) + 1
				t.Errorf("%s:%d: %s is blocked by the Content-Security-Policy", path, line, f.what)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Golden renders of every page, from fixed data. They are what a reviewer
// reads to see a template change, and they fail on an accidental one. Run
// `go test ./internal/ui -run TestTemplates_Golden -update` to accept a
// deliberate change.
func TestTemplates_Golden(t *testing.T) {
	pages, err := parsePages()
	if err != nil {
		t.Fatalf("parsePages: %v", err)
	}
	long := time.Date(2020, 3, 14, 15, 9, 26, 0, time.UTC)
	digest := "sha256:4b825dc642cb6eb9a060e54bf8d69288fbee4904cafebabedeadbeefcafebabe"
	base := chrome{Title: "t", Who: "user:alice", SignOut: true, Version: "0.0.0-test"}
	team := repoView{Name: "team/app", Type: "hosted", Protocol: "oci", ManagedBy: "config", Href: "/ui/r/team/app"}

	cases := map[string]struct {
		page string
		data any
	}{
		"repos": {"repos", struct {
			chrome
			Repos []tile
		}{base, []tile{
			{repoView: team},
			{repoView: repoView{Name: "hub", Type: "proxy", Protocol: "oci", Upstream: true, Href: "/ui/r/hub"}, Detail: "caches https://registry-1.docker.io"},
			{repoView: repoView{Name: "python", Type: "virtual", Protocol: "pypi", Href: "/ui/r/python"}, Detail: "resolves internal-py → pypi"},
		}}},
		"repos-empty": {"repos", struct {
			chrome
			Repos []tile
		}{base, nil}},
		"repo": {"repo", repoPage{chrome: base, Repo: team,
			Packages: []link{{Name: "web", Href: "/ui/r/team/app/-/package?name=web"}},
			More:     "/ui/r/team/app?cursor=web"}},
		"repo-proxy": {"repo", repoPage{chrome: base,
			Repo:     repoView{Name: "hub", Type: "proxy", Protocol: "oci", Upstream: true},
			Upstream: "https://registry-1.docker.io"}},
		"package-oci": {"package", packagePage{chrome: base, Repo: team, Package: "web",
			Tags: []tagRow{{Name: "1.0", Digest: digest, Href: "/ui/r/team/app/-/artifact?digest=" + digest}}}},
		"package-pypi": {"package", packagePage{chrome: base,
			Repo: repoView{Name: "internal-py", Type: "hosted", Protocol: "pypi"}, Package: "demo",
			Versions: []versionGroup{{Version: "1.0", Files: []fileRow{{Filename: "demo-1.0.tar.gz", Size: 2048, Href: "/x"}}}}}},
		"artifact": {"artifact", artifactPage{chrome: base, Repo: "team/app", RepoHref: "/ui/r/team/app",
			Digest: digest, Size: 1234, MediaType: "application/vnd.oci.image.manifest.v1+json", Kind: "oci-manifest",
			HasIdentity: true, Owners: []string{"repository:team/app"}, CreatedAt: long,
			Provenance: []lifecycle.Provenance{{Provenance: model.Provenance{SourceType: "oci-push", SourceRef: "https://git.example.com/app", BuildRef: "4b825dc"}, RecordedAt: long}},
			StateRepos: []string{"team/app"},
			Stages: []stageRow{
				{Name: "dev", Cells: []stateCell{{Action: "approve", Actor: "user:alice", At: long}}},
				{Name: "prod", Cells: []stateCell{{}}},
			},
			Evidence: []evidence.Evidence{{Type: model.EvidenceSBOM, MediaType: "application/spdx+json", Size: 512, CreatedBy: "token:ci", CreatedAt: long}},
			Children: []childRow{{Digest: digest, Size: 99, Href: "/y"}},
			Document: "{\n  \"schemaVersion\": 2\n}"}},
		"artifact-upstream": {"artifact", artifactPage{chrome: base, Repo: "hub", RepoHref: "/ui/r/hub",
			Digest: digest, Size: 1234, Upstream: true}},
		"notfound": {"notfound", base},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := pages[tc.page].ExecuteTemplate(&buf, "layout", tc.data); err != nil {
				t.Fatalf("rendering: %v", err)
			}
			// The asset version changes with every static file; the golden
			// files should not.
			got := strings.ReplaceAll(buf.String(), web.Version, "VERSION")
			path := filepath.Join("testdata", name+".golden.html")
			if *update {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) //nolint:gosec // G304: a fixed name under testdata
			if err != nil {
				t.Fatalf("reading %s (run with -update to create it): %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("%s differs from the golden file; run with -update if the change is intended\n--- got\n%s", path, got)
			}
		})
	}
}

func TestShortDigest(t *testing.T) {
	tests := map[string]string{
		"sha256:4b825dc642cb6eb9a060e54bf8d69288fbee4904": "sha256:4b825dc642cb",
		"sha256:abc": "sha256:abc",
		"plain":      "plain",
	}
	for in, want := range tests {
		if got := shortDigest(in); got != want {
			t.Errorf("shortDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB"}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPackageLabel_NamesARootImageAfterItsRepository(t *testing.T) {
	oci := model.Repository{Name: "demo/app", Protocol: model.ProtocolOCI}
	py := model.Repository{Name: "py", Protocol: model.ProtocolPyPI}
	tests := []struct {
		repo model.Repository
		in   string
		want string
	}{
		{oci, "_", "demo/app"},
		{oci, "web", "web"},
		{py, "_", "_"},
	}
	for _, tc := range tests {
		if got := packageLabel(tc.repo, tc.in); got != tc.want {
			t.Errorf("packageLabel(%s, %q) = %q, want %q", tc.repo.Name, tc.in, got, tc.want)
		}
	}
}
