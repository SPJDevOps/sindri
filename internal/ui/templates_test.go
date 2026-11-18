package ui

import (
	"bytes"
	"encoding/json"
	"flag"
	"html/template"
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

// panelFor builds the "Use this" section the way the pages do, from the same
// builders, so a golden shows real commands. Tabs link to path.
func panelFor(title, path string, in cmdInput, steps []cmdStep, notes []string) *usePanel {
	in.Client = pickClient(in.Protocol, in.Client)
	p := &usePanel{Title: title, Steps: steps, Notes: notes, Fallback: in.Fallback}
	for _, c := range clientsFor(in.Protocol) {
		p.Tabs = append(p.Tabs, tabLink{Label: c, Href: path + "?client=" + c, Current: c == in.Client})
	}
	return p
}

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

// The Content-Security-Policy lets a form submit to this server (form-action
// 'self'). That is safe only while every form is a GET: the UI has no write
// path, and the session cookie is honoured on GET and HEAD alone. A form with
// no method counts as a GET to a browser, but the template must say so, so
// that a change to POST is a visible edit and not a missing attribute (ADR 21).
// The form must be a GET, or a dialog form, which never submits.
func TestTemplates_EveryFormIsAnExplicitGET(t *testing.T) {
	form := regexp.MustCompile(`(?is)<form\b[^>]*>`)
	// method="dialog" closes the <dialog> it sits in and sends nothing, so it
	// needs no form-action; it is the only other method a template may use.
	method := regexp.MustCompile(`(?i)\smethod="(?:get|dialog)"`)
	// A button's formmethod or formaction overrides its form's, so a GET form
	// could still post through one; only formmethod="get" is allowed.
	override := regexp.MustCompile(`(?i)\sform(?:method|action)\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	allowed := regexp.MustCompile(`(?i)^\sformmethod\s*=\s*["']?get["']?$`)
	forms := 0
	err := fs.WalkDir(web.Templates(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web.Templates(), path)
		if err != nil {
			return err
		}
		for _, loc := range override.FindAllIndex(b, -1) {
			if allowed.Match(b[loc[0]:loc[1]]) {
				continue
			}
			line := bytes.Count(b[:loc[0]], []byte("\n")) + 1
			t.Errorf("%s:%d: formmethod and formaction may not override a form's GET: %s", path, line, b[loc[0]:loc[1]])
		}
		for _, loc := range form.FindAllIndex(b, -1) {
			forms++
			if tag := b[loc[0]:loc[1]]; !method.Match(tag) {
				line := bytes.Count(b[:loc[0]], []byte("\n")) + 1
				t.Errorf("%s:%d: a <form> must say method=\"get\" (or \"dialog\", which sends nothing): %s", path, line, tag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if forms == 0 {
		t.Error("no <form> found in any template; the test measures nothing")
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
	base := chrome{Title: "t", Who: "user:alice", SignOut: true, Version: "0.0.0-test", DocsURL: "https://docs.example.test/sindri/"}
	adminBase := base
	adminBase.Admin = true
	team := repoView{Name: "team/app", Type: "hosted", Protocol: "oci", ManagedBy: "config", Href: "/ui/r/team/app"}

	const base0 = "https://sindri.example.test"
	ociIn := cmdInput{Base: base0, Repo: "team/app", Protocol: model.ProtocolOCI, Kind: model.RepoTypeHosted}
	pullIn := ociIn
	pullIn.Package, pullIn.Digest, pullIn.Tag = "web", digest, "1.0"
	pypiIn := cmdInput{Base: base0, Repo: "internal-py", Protocol: model.ProtocolPyPI, Kind: model.RepoTypeHosted, Package: "demo"}
	npmIn := cmdInput{Base: base0, Repo: "internal-npm", Protocol: model.ProtocolNPM, Kind: model.RepoTypeHosted, Package: "@acme/ui", Version: "1.1.0"}
	virtIn := cmdInput{Base: base0, Repo: "python", Protocol: model.ProtocolPyPI, Kind: model.RepoTypeVirtual, Client: "uv"}
	hubIn := cmdInput{Base: "http://sindri.test:8080", Repo: "hub", Protocol: model.ProtocolOCI, Kind: model.RepoTypeProxy, Package: "library/alpine", Digest: digest, Fallback: true}
	manage := ManageCommands(manageInput{Base: base0, Repo: "team/app", Digest: digest, PromoteTo: "prod", RevokeFrom: "dev", Rescan: true})

	cases := map[string]struct {
		page string
		data any
	}{
		"repos": {"repos", reposPage{chrome: base, Total: 3, Matching: 3, From: 1, To: 3,
			Table: false, Types: options(repoTypes, ""), Protos: options(repoProtocols, ""),
			TableHref: "/ui/?view=table", TilesHref: "/ui/?view=tiles", ClearHref: "/ui/",
			Groups: []repoGroup{
				{Rows: []repoRow{
					{repoView: repoView{Name: "hub", Type: "proxy", Protocol: "oci", Upstream: true, Href: "/ui/r/hub"}, Detail: "caches registry-1.docker.io",
						Stats: &rowStats{Packages: 4, LastPublished: long}},
					{repoView: repoView{Name: "python", Type: "virtual", Protocol: "pypi", Href: "/ui/r/python"}, Detail: "resolves 2 members"},
				}, Total: 2},
				{Label: "team", Rows: []repoRow{{repoView: team, Stats: &rowStats{Packages: 1}}}, Total: 1},
			}}},
		"repos-table": {"repos", reposPage{chrome: base, Total: 500, Matching: 500, From: 1, To: 200,
			Table: true, Types: options(repoTypes, ""), Protos: options(repoProtocols, ""),
			TableHref: "/ui/?view=table", TilesHref: "/ui/?view=tiles", ClearHref: "/ui/",
			Pager: &pagerView{Page: 1, Pages: 3, Next: "/ui/?page=2"},
			Groups: []repoGroup{
				{Rows: []repoRow{
					{repoView: repoView{Name: "hub", Type: "proxy", Protocol: "oci", ManagedBy: "config", Upstream: true, Href: "/ui/r/hub"}, Detail: "caches registry-1.docker.io",
						Stats: &rowStats{Packages: 4, LastPublished: long}},
					{repoView: repoView{Name: "python", Type: "virtual", Protocol: "pypi", ManagedBy: "config", Href: "/ui/r/python"}, Detail: "resolves 2 members"},
				}, Total: 2},
				// team continues on page 2, so its pill says how many of it this page shows.
				{Label: "team", Total: 5, Rows: []repoRow{
					{repoView: repoView{Name: "team/app", Type: "hosted", Protocol: "oci", ManagedBy: "config", Href: "/ui/r/team/app"}, Stats: &rowStats{Packages: 12, LastPublished: long}},
					{repoView: repoView{Name: "team/lib", Type: "hosted", Protocol: "pypi", ManagedBy: "api", Locked: true, Href: "/ui/r/team/lib"}, Stats: &rowStats{}},
				}},
			}}},
		"repos-filtered": {"repos", reposPage{chrome: base, Total: 500, Matching: 2, From: 1, To: 2,
			Filter: repoFilter{Q: "team", Type: "hosted", View: "table"}, Table: true,
			Types: options(repoTypes, "hosted"), Protos: options(repoProtocols, ""),
			TableHref: "/ui/?q=team&type=hosted&view=table", TilesHref: "/ui/?q=team&type=hosted&view=tiles", ClearHref: "/ui/?view=table",
			Groups: []repoGroup{{Label: "team", Total: 2, Rows: []repoRow{
				{repoView: repoView{Name: "team/app", Type: "hosted", Protocol: "oci", ManagedBy: "config", Href: "/ui/r/team/app"}, Stats: &rowStats{Packages: 12, LastPublished: long}},
				{repoView: repoView{Name: "team/lib", Type: "hosted", Protocol: "pypi", ManagedBy: "api", Href: "/ui/r/team/lib"}, Stats: &rowStats{}},
			}}}}},
		"repos-nomatch": {"repos", reposPage{chrome: base, Total: 500, Matching: 0,
			Filter: repoFilter{Q: "zzz"}, Types: options(repoTypes, ""), Protos: options(repoProtocols, ""),
			ClearHref: "/ui/"}},
		"repos-empty": {"repos", reposPage{chrome: base}},
		"repo": {"repo", repoPage{chrome: base, Repo: team,
			Facts: repoFacts{IsHosted: true, Stages: []string{"dev", "prod"}},
			Sorts: packageSorts, Total: 120, From: 1, To: 2, ClearHref: "/ui/r/team/app",
			Pager: &pagerView{Page: 1, Pages: 3, Next: "/ui/r/team/app?page=2"},
			Rows: []pkgRow{
				{Name: "web", Href: "/ui/r/team/app/-/package?name=web", Latest: "1.2", Versions: 14, Published: long, HasSeverity: true,
					Severity: severityView{Scanned: true, Counts: []severityCount{{Severity: "critical", Class: "sev-critical", N: 3}}}},
				{Name: "worker", Href: "/ui/r/team/app/-/package?name=worker", Latest: "latest", Versions: 1, HasSeverity: true},
				{Name: "api", Href: "/ui/r/team/app/-/package?name=api", Latest: "2.0", Versions: 6, Published: long, HasSeverity: true,
					Severity: severityView{Scanned: true}},
			}}},
		"repo-empty": {"repo", repoPage{chrome: base, Repo: team, Sorts: packageSorts, ClearHref: "/ui/r/team/app",
			Facts:   repoFacts{IsHosted: true, NoStages: true},
			Publish: PublishCommands(ociIn)}},
		"repo-filtered": {"repo", repoPage{chrome: base, Repo: team, Sorts: packageSorts, ClearHref: "/ui/r/team/app",
			Facts:  repoFacts{IsHosted: true, Stages: []string{"dev", "prod"}},
			Filter: pkgFilter{Q: "zzz", Sort: sortName}}},
		"repo-npm": {"repo", repoPage{chrome: base, Sorts: packageSorts, ClearHref: "/ui/r/internal-npm",
			Repo:   repoView{Name: "internal-npm", Type: "hosted", Protocol: "npm", ManagedBy: "api", Href: "/ui/r/internal-npm"},
			Facts:  repoFacts{IsHosted: true, Stages: []string{"dev", "prod"}},
			Filter: pkgFilter{Sort: sortPublished}, Total: 1, From: 1, To: 1,
			Rows: []pkgRow{{Name: "@acme/ui", Href: "/ui/r/internal-npm/-/package?name=%40acme%2Fui", Latest: "1.1.0", Versions: 2, Published: long, HasSeverity: true}}}},
		"repo-virtual": {"repo", repoPage{chrome: base, IsVirtual: true,
			Repo:    repoView{Name: "python", Type: "virtual", Protocol: "pypi", Href: "/ui/r/python"},
			Members: []link{{Name: "internal-py", Type: "hosted", Href: "/ui/r/internal-py"}, {Name: "pypi", Type: "proxy", Href: "/ui/r/pypi"}},
			Use:     panelFor("Set up your client", "/ui/r/python", virtIn, SetupCommands(virtIn), nil)}},
		"repo-proxy": {"repo", repoPage{chrome: base, Sorts: packageSorts, ClearHref: "/ui/r/hub",
			Repo:     repoView{Name: "hub", Type: "proxy", Protocol: "oci", ManagedBy: "config", Upstream: true},
			Upstream: "https://registry-1.docker.io",
			Facts:    repoFacts{IsProxy: true, Entries: 12, LastFetch: long},
			Total:    1, From: 1, To: 1,
			Rows: []pkgRow{{Name: "library/alpine", Href: "/ui/r/hub/-/package?name=library%2Falpine", Latest: "", Versions: 3}}}},
		"repo-proxy-empty": {"repo", repoPage{chrome: base, Sorts: packageSorts, ClearHref: "/ui/r/hub",
			Repo:     repoView{Name: "hub", Type: "proxy", Protocol: "oci", ManagedBy: "config", Upstream: true},
			Upstream: "https://registry-1.docker.io", Facts: repoFacts{IsProxy: true}}},
		"package-oci": {"package", packagePage{chrome: base, Repo: team, Package: "web",
			Tags: []tagRow{
				{Name: "1.0", Digest: digest, Href: "/ui/r/team/app/-/artifact?digest=" + digest, Shared: 2, Published: long, Owners: []string{"repository:team/app"},
					Stage: "prod", Size: 5 << 20, Severity: severityView{Scanned: true, Counts: []severityCount{{Severity: "critical", Class: "sev-critical", N: 1}, {Severity: "low", Class: "sev-low", N: 7}}}},
				{Name: "latest", Digest: digest, Href: "/ui/r/team/app/-/artifact?digest=" + digest, Shared: 2, Published: long, Owners: []string{"repository:team/app"},
					Stage: "prod", Size: 5 << 20, Severity: severityView{Scanned: true, Counts: []severityCount{{Severity: "critical", Class: "sev-critical", N: 1}, {Severity: "low", Class: "sev-low", N: 7}}}},
				{Name: "2.0-rc1", Digest: digest[:len(digest)-1] + "0", Href: "/ui/r/team/app/-/artifact?digest=" + digest, Shared: 1, Published: long, Size: 6 << 20, Severity: severityView{Scanned: true}},
				{Name: "scratch", Digest: digest[:len(digest)-1] + "1", Href: "/ui/r/team/app/-/artifact?digest=" + digest, Shared: 1, Size: 1 << 20},
			},
			More: "/ui/r/team/app/-/package?name=web&cursor=scratch"}},
		"package-oci-proxy": {"package", packagePage{chrome: base,
			Repo: repoView{Name: "hub", Type: "proxy", Protocol: "oci", Upstream: true, ManagedBy: "config"}, Package: "library/alpine",
			Tags: []tagRow{{Name: "3.20", Digest: digest, Href: "/ui/r/hub/-/artifact?digest=" + digest, Shared: 1, Size: 3 << 20}}}},
		"package-pypi": {"package", packagePage{chrome: base,
			Repo: repoView{Name: "internal-py", Type: "hosted", Protocol: "pypi"}, Package: "demo",
			Versions: []versionRow{
				{Version: "1.0", Published: long, Owners: []string{"repository:internal-py"}, Stage: "dev", Size: 2048 + 4096,
					Severity: severityView{Scanned: false, Counts: []severityCount{{Severity: "high", Class: "sev-high", N: 2}}},
					Files:    []fileRow{{Filename: "demo-1.0.tar.gz", Size: 2048, Href: "/x"}, {Filename: "demo-1.0-py3-none-any.whl", Size: 4096, Href: "/y"}}},
			},
			Use: panelFor("Install", "/ui/r/internal-py/-/package", pypiIn, InstallCommands(pypiIn), installNotes(pypiIn))}},
		"package-npm": {"package", packagePage{chrome: base,
			Repo: repoView{Name: "internal-npm", Type: "hosted", Protocol: "npm"}, Package: "@acme/ui",
			Versions: []versionRow{
				{Version: "1.1.0", Tags: []string{"latest"}, Published: long, Owners: []string{"repository:internal-npm"}, Stage: "prod", Size: 4096,
					Severity: severityView{Scanned: true}, Files: []fileRow{{Filename: "ui-1.1.0.tgz", Size: 4096, Href: "/x"}}},
				{Version: "2.0.0-beta.1", Tags: []string{"beta"}, Published: long, Size: 3000, Files: []fileRow{{Filename: "ui-2.0.0-beta.1.tgz", Size: 3000, Href: "/z"}}},
				{Version: "1.0.0", Deprecated: "use 1.1.0, this release leaks a handle", Published: long, Owners: []string{"repository:internal-npm"}, Size: 2048,
					Severity: severityView{Scanned: true, Counts: []severityCount{{Severity: "medium", Class: "sev-medium", N: 4}}},
					Files:    []fileRow{{Filename: "ui-1.0.0.tgz", Size: 2048, Href: "/ui/r/internal-npm/-/artifact?digest=" + digest + "&image=%40acme%2Fui"}}},
			},
			Use: panelFor("Install", "/ui/r/internal-npm/-/package", npmIn, InstallCommands(npmIn), installNotes(npmIn))}},
		"artifact": {"artifact", artifactPage{chrome: base, Repo: "team/app", RepoHref: "/ui/r/team/app",
			Name: "web:1.0",
			Verdict: &verdictView{Stage: "prod", ScannedAt: long, NewFindings: true,
				Severity: severityView{Scanned: true, Counts: []severityCount{{Severity: "critical", Class: "sev-critical", N: 1}}}},
			Names:  []nameView{{Label: "web:1.0", Href: "/ui/r/team/app/-/package?name=web"}, {Label: "web:latest", Href: "/ui/r/team/app/-/package?name=web"}},
			AlsoIn: []link{{Name: "team/mirror", Type: "hosted", Href: "/ui/r/team/mirror"}},
			Digest: digest, Size: 1234, MediaType: "application/vnd.oci.image.manifest.v1+json", Kind: "oci-manifest",
			HasIdentity: true, Owners: []string{"repository:team/app"}, CreatedAt: long,
			Provenance: []lifecycle.Provenance{{Provenance: model.Provenance{SourceType: "oci-push", SourceRef: "https://git.example.com/app", BuildRef: "4b825dc"}, RecordedAt: long}},
			StateRepos: []string{"team/app"},
			Stages: []stageRow{
				{Name: "dev", Cells: []stateCell{{Action: "approve", Actor: "user:alice", At: long}}},
				{Name: "prod", Cells: []stateCell{{}}},
			},
			Evidence: []evidenceRow{
				newEvidenceRow(evidence.Evidence{Type: model.EvidenceSBOM, MediaType: "application/spdx+json", Size: 512, CreatedBy: "token:ci", CreatedAt: long}),
				newEvidenceRow(evidence.Evidence{Type: model.EvidenceScan, MediaType: "application/vnd.aquasec.trivy.report+json", Size: 2048, CreatedBy: "system:scanner", CreatedAt: long,
					Metadata: json.RawMessage(`{"producer":"sindri","platform":"linux/amd64","severity_counts":{"CRITICAL":1,"HIGH":3,"MEDIUM":0,"LOW":7,"UNKNOWN":0}}`)}),
				newEvidenceRow(evidence.Evidence{Type: model.EvidenceScan, MediaType: "application/vnd.aquasec.trivy.report+json", Size: 1024, CreatedBy: "system:scanner", CreatedAt: long,
					Metadata: json.RawMessage(`{"producer":"sindri","severity_counts":{"CRITICAL":0,"HIGH":0,"MEDIUM":0,"LOW":0,"UNKNOWN":0}}`)}),
				newEvidenceRow(evidence.Evidence{Type: model.EvidenceScan, MediaType: "application/json", Size: 64, CreatedBy: "token:ci", CreatedAt: long, Metadata: json.RawMessage(`{}`)}),
			},
			Vulns: newVulnPanels([]evidence.Evidence{
				{ID: "ev-1", Type: model.EvidenceScan, CreatedAt: long, Metadata: json.RawMessage(`{"producer":"sindri","platform":"linux/amd64","db_updated_at":"2020-03-14T06:00:00Z","severity_counts":{"CRITICAL":1,"HIGH":0,"MEDIUM":0,"LOW":1,"UNKNOWN":0},"vulnerabilities":[{"id":"CVE-2020-0002","severity":"LOW","package":"zlib","installed":"1.2.8"},{"id":"CVE-2020-0001","severity":"CRITICAL","package":"openssl","installed":"1.1.1","fixed":"1.1.1k"}],"new_vulnerabilities":["CVE-2020-0001"]}`)},
				{ID: "ev-2", Type: model.EvidenceScan, CreatedAt: long, Metadata: json.RawMessage(`{"producer":"sindri","platform":"linux/arm64","severity_counts":{"CRITICAL":0,"HIGH":0,"MEDIUM":0,"LOW":0,"UNKNOWN":0},"vulnerabilities":[]}`)},
			}),
			Deliveries: []deliveryRow{{Webhook: "chat", Event: "finding.new", State: "delivered", Dot: "ok", Status: "200", At: long}},
			Layers: []layerRow{
				{Digest: digest, Size: 1234, Href: "/c", IsConfig: true},
				{Digest: digest[:len(digest)-1] + "0", Size: 3 << 20, Href: "/l1", Bucket: 3, Step: "/bin/sh -c apk add --no-cache ca-certificates"},
				{Digest: digest[:len(digest)-1] + "1", Size: 7 << 20, Href: "/l2", Bucket: 7},
			},
			Document: "{\n  \"schemaVersion\": 2\n}",
			Use:      panelFor("Pull", "/ui/r/team/app/-/artifact", pullIn, PullCommands(pullIn), nil),
			Manage:   manage, TokenDocs: "https://docs.example.test/sindri/quickstart.html"}},
		"artifact-index": {"artifact", artifactPage{chrome: base, Repo: "team/app", RepoHref: "/ui/r/team/app",
			Name: "web:2.0", Verdict: &verdictView{Stage: "dev", Revoked: true, Severity: severityView{}},
			Names:  []nameView{{Label: "web:2.0", Href: "/ui/r/team/app/-/package?name=web"}},
			Digest: digest, Size: 480, MediaType: "application/vnd.oci.image.index.v1+json", Kind: "oci-index",
			HasIdentity: true, Owners: []string{"repository:team/app"}, CreatedAt: long, IsIndex: true,
			StateRepos: []string{"team/app"},
			Stages:     []stageRow{{Name: "dev", Cells: []stateCell{{Action: "revoke", Actor: "user:alice", At: long}}}},
			Platforms: []platformRow{
				{Platform: "linux/amd64", Digest: digest[:len(digest)-1] + "0", Href: "/a", Size: 5 << 20, HasVerdict: true, Stage: "dev",
					Severity: severityView{Scanned: true, Counts: []severityCount{{Severity: "high", Class: "sev-high", N: 2}}}},
				{Platform: "linux/arm64/v8", Digest: digest[:len(digest)-1] + "1", Href: "/b", Size: 5 << 20, HasVerdict: true, Severity: severityView{Scanned: true}},
				{Platform: "no platform", Digest: digest[:len(digest)-1] + "2", Href: "/c", Size: 800, HasVerdict: true},
			},
			Document: "{\n  \"schemaVersion\": 2\n}",
			Use:      panelFor("Pull", "/ui/r/team/app/-/artifact", pullIn, PullCommands(pullIn), nil)}},
		"artifact-pypi": {"artifact", artifactPage{chrome: base, Repo: "internal-py", RepoHref: "/ui/r/internal-py",
			Name:    "demo 1.0",
			Verdict: &verdictView{Stage: "dev", ScannedAt: long, Severity: severityView{Scanned: true}},
			Names:   []nameView{{Label: "demo 1.0", Href: "/ui/r/internal-py/-/package?name=demo", Detail: "demo-1.0.tar.gz"}},
			Digest:  digest, Size: 2048, Kind: "pypi-distribution",
			HasIdentity: true, Owners: []string{"repository:internal-py"}, CreatedAt: long,
			StateRepos: []string{"internal-py"},
			Stages:     []stageRow{{Name: "dev", Cells: []stateCell{{Action: "approve", Actor: "user:alice", At: long}}}},
			Evidence:   []evidenceRow{},
			Use:        panelFor("Install", "/ui/r/internal-py/-/artifact", pypiIn, InstallCommands(pypiIn), installNotes(pypiIn))}},
		"artifact-npm": {"artifact", artifactPage{chrome: base, Repo: "internal-npm", RepoHref: "/ui/r/internal-npm",
			Name:    "@acme/ui 1.1.0",
			Verdict: &verdictView{Severity: severityView{}},
			Names:   []nameView{{Label: "@acme/ui 1.1.0", Href: "/ui/r/internal-npm/-/package?name=%40acme%2Fui", Detail: "ui-1.1.0.tgz"}},
			Digest:  digest, Size: 4096, Kind: "npm-tarball",
			HasIdentity: true, Owners: []string{"repository:internal-npm"}, CreatedAt: long,
			StateRepos: []string{"internal-npm"},
			Stages:     []stageRow{{Name: "dev", Cells: []stateCell{{}}}},
			Provenance: []lifecycle.Provenance{{Provenance: model.Provenance{SourceType: "npm-publish"}, RecordedAt: long}},
			Use:        panelFor("Install", "/ui/r/internal-npm/-/artifact", npmIn, InstallCommands(npmIn), installNotes(npmIn))}},
		"artifact-upstream": {"artifact", artifactPage{chrome: base, Repo: "hub", RepoHref: "/ui/r/hub",
			Digest: digest, Size: 1234, Upstream: true,
			Use: panelFor("Pull", "/ui/r/hub/-/artifact", hubIn, PullCommands(hubIn), append([]string{"Pulls through the proxy, which fetches from the upstream on a miss."}, ociNotes(hubIn)...))}},
		"notfound": {"notfound", base},
		"error": {"error", struct {
			chrome
			RequestID string
		}{base, "7f3c9a2e-4d1b-4c8e-9a55-0b1f6d2e8a34"}},
		"system": {"system", systemPage{chrome: adminBase, Configured: true, StatusClass: "bad", StatusText: "1 webhook failing",
			StatusHref: "/ui/system/notifications?state=failed", Examples: examplePayloads,
			Hooks: []hookRow{{Name: "chat", Events: "finding.new, lifecycle.promoted", Target: "https://hooks.example.com", Failed: 1, LogHref: "/ui/system/notifications?webhook=chat"}}}},
		"system-empty": {"system", systemPage{chrome: adminBase, StatusClass: "none", StatusText: "No webhooks configured",
			StatusHref: "/ui/system/notifications", Examples: examplePayloads}},
		"deliveries": {"deliveries", deliveriesPage{chrome: adminBase, State: "failed", Limit: 100,
			Filters: []stateFilter{{"all", "/ui/system/notifications", false}, {"failed", "/ui/system/notifications?state=failed", true}},
			Rows: []deliveryRow{
				{ID: 7, Webhook: "chat", Event: "finding.new", Digest: digest, Href: "/ui/r/team/app/-/artifact?digest=" + digest, Repository: "team/app", State: "failed", Dot: "bad",
					Status: "503", Attempts: 3, At: long, Error: "receiver chat answered 503",
					Redeliver: `curl -X POST -H "Authorization: Bearer $TOKEN" https://sindri.example/api/v1/notifications/deliveries/7/redeliver`},
				{ID: 8, Webhook: "chat", Event: "lifecycle.promoted", Digest: digest, Repository: "team/app", State: "pending", Dot: "warn",
					Status: "502", Attempts: 1, At: long, NextRetry: long.Add(time.Minute), Error: "receiver chat answered 502"},
			}}},
		"deliveries-empty": {"deliveries", deliveriesPage{chrome: adminBase, Limit: 100}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if page, ok := tc.data.(artifactPage); ok {
				// The navigation is derived from what the page draws, as in the handler.
				page.Sections = sectionsOf(&page)
				tc.data = page
			}
			var buf bytes.Buffer
			if err := pages[tc.page].ExecuteTemplate(&buf, "layout", tc.data); err != nil {
				t.Fatalf("rendering: %v", err)
			}
			compareGolden(t, name, buf.String())
		})
	}
}

// The header is shown as a fragment too, so a change to it reads as a change
// to a few lines and not to every page. The narrow header is the native menu
// that takes the place of the links below 720px; the stylesheet decides which
// one is shown, so both are always in the markup.
func TestTemplates_GoldenHeader(t *testing.T) {
	pages, err := parsePages()
	if err != nil {
		t.Fatalf("parsePages: %v", err)
	}
	user := chrome{Title: "t", Who: "user:alice", SignOut: true, Version: "0.0.0-test", DocsURL: "https://docs.example.test/sindri/"}
	admin := user
	admin.Admin = true
	token := chrome{Title: "t", Who: "token:ci", Version: "0.0.0-test"}

	cases := map[string]struct {
		fragment string
		data     chrome
	}{
		"header-admin":        {"header", admin},
		"header-user":         {"header", user},
		"header-token":        {"header", token},
		"header-narrow-admin": {"menu", admin},
		"header-narrow-user":  {"menu", user},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := pages["notfound"].ExecuteTemplate(&buf, tc.fragment, tc.data); err != nil {
				t.Fatalf("rendering: %v", err)
			}
			compareGolden(t, name, buf.String())
		})
	}
}

// compareGolden checks rendered markup against testdata/<name>.golden.html,
// or rewrites it under -update.
func compareGolden(t *testing.T, name, rendered string) {
	t.Helper()
	// The asset version changes with every static file; the golden files
	// should not.
	got := strings.ReplaceAll(rendered, web.Version, "VERSION")
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

// The reference page shows every component in every state. It has no route:
// it is rendered here, so a reviewer reads a component change in the golden
// file and a component cannot exist without being shown. Accept a deliberate
// change with `go test ./internal/ui -run TestReference_Golden -update`.
func TestReference_Golden(t *testing.T) {
	page, err := template.New("reference").Funcs(funcs).ParseFS(web.Templates(), "layout.html", "reference.html")
	if err != nil {
		t.Fatalf("parsing the reference page: %v", err)
	}
	ref := cmdInput{Base: "http://sindri.test:8080", Repo: "internal", Protocol: model.ProtocolPyPI, Kind: model.RepoTypeHosted, Package: "demo", Version: "1.0", Client: "poetry", Fallback: true}
	data := struct {
		chrome
		Use                        *usePanel
		Findings, Clean, Unscanned severityView
	}{
		chrome{Title: "Design reference", Who: "user:alice", SignOut: true, Version: "0.0.0-test", DocsURL: "https://docs.example.test/sindri/"},
		panelFor("Install", "#use", ref, InstallCommands(ref), installNotes(ref)),
		severityView{Scanned: true, Counts: []severityCount{{Severity: "critical", Class: "sev-critical", N: 1}, {Severity: "high", Class: "sev-high", N: 3}, {Severity: "low", Class: "sev-low", N: 7}}},
		severityView{Scanned: true},
		severityView{},
	}
	var buf bytes.Buffer
	if err := page.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	got := strings.ReplaceAll(buf.String(), web.Version, "VERSION")
	path := filepath.Join("testdata", "reference.golden.html")
	if *update {
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
}
