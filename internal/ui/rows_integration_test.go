//go:build integration

package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// sindriScan records a scan as Sindri's own worker writes it. The evidence API
// refuses a producer from a client, so the row goes in directly; the blob it
// names is the subject's own, which is all the foreign key asks.
func (e *env) sindriScan(d model.Digest, platform string, counts map[string]int) {
	e.t.Helper()
	meta := fmt.Sprintf(`{"producer":"sindri","vulnerabilities":[],"severity_counts":{"CRITICAL":%d,"HIGH":%d,"MEDIUM":%d,"LOW":%d,"UNKNOWN":%d}`,
		counts["CRITICAL"], counts["HIGH"], counts["MEDIUM"], counts["LOW"], counts["UNKNOWN"])
	if platform != "" {
		meta += fmt.Sprintf(`,"platform":%q,"platforms":[%q]`, platform, platform)
	}
	meta += "}"
	if _, err := e.pool.Exec(e.t.Context(), `
		INSERT INTO evidence (subject_digest, type, media_type, blob_digest, metadata, created_by)
		VALUES ($1, 'scan', 'application/json', $1, $2::jsonb, 'system:scanner')`, d.String(), meta); err != nil {
		e.t.Fatalf("recording a scan of %s: %v", d, err)
	}
}

// indexScan records the scan of one platform of an index as the worker writes
// it: on the index digest, with the platform it covers and the full list of
// platforms the job meant to scan (ADR 30).
func (e *env) indexScan(d model.Digest, platform string, platforms []string, counts map[string]int) {
	e.t.Helper()
	list, err := json.Marshal(platforms)
	if err != nil {
		e.t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"producer":"sindri","platform":%q,"platforms":%s,"vulnerabilities":[],"severity_counts":{"CRITICAL":%d,"HIGH":%d,"MEDIUM":%d,"LOW":%d,"UNKNOWN":%d}}`,
		platform, list, counts["CRITICAL"], counts["HIGH"], counts["MEDIUM"], counts["LOW"], counts["UNKNOWN"])
	if _, err := e.pool.Exec(e.t.Context(), `
		INSERT INTO evidence (subject_digest, type, media_type, blob_digest, metadata, created_by)
		VALUES ($1, 'scan', 'application/json', $1, $2::jsonb, 'system:scanner')`, d.String(), meta); err != nil {
		e.t.Fatalf("recording a scan of %s: %v", d, err)
	}
}

// pushIndex pushes an index over two fresh single-image children under tag and
// returns the index digest.
func (e *env) pushIndex(repo, image, tag, seed string) model.Digest {
	e.t.Helper()
	var descs []ocispec.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		d := e.push(repo, image, seed+"-"+arch, seed+"-"+arch)
		info, err := e.reg.StatBlob(e.t.Context(), repo, d)
		if err != nil {
			e.t.Fatalf("StatBlob: %v", err)
		}
		descs = append(descs, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digestOf(d), Size: info.Size,
			Platform: &ocispec.Platform{OS: "linux", Architecture: arch}})
	}
	index := ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: descs}
	index.SchemaVersion = 2
	body, err := json.Marshal(index)
	if err != nil {
		e.t.Fatal(err)
	}
	idx, err := e.reg.PutManifest(e.t.Context(), repo, image, tag, ocispec.MediaTypeImageIndex, body, model.Provenance{SourceType: "oci-push"})
	if err != nil {
		e.t.Fatalf("PutManifest index: %v", err)
	}
	return idx.Digest
}

// rowFor returns the table row of body whose first cell holds name.
func rowFor(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<tr>\s*<td>[^\n]*?>` + regexp.QuoteMeta(name) + `<.*?</tr>`).FindString(body)
	if m == "" {
		t.Fatalf("no row for %q in:\n%s", name, body)
	}
	return m
}

func TestUI_RepositoryRowsShowLatestTagCountAndWorstSeverity(t *testing.T) {
	e := newEnv(t)
	d1 := e.push("team-a", "app", "1.0", "one")
	time.Sleep(10 * time.Millisecond) // first-publish times must differ
	d2 := e.push("team-a", "app", "2.0", "two")
	e.push("team-a", "bare", "1.0", "three")
	e.sindriScan(d1, "", map[string]int{"LOW": 2})
	e.sindriScan(d2, "", map[string]int{"CRITICAL": 1, "HIGH": 4})

	body := e.get("alice", "/ui/r/team-a").body
	app := rowFor(t, body, "app")
	for _, want := range []string{">2.0<", `<td class="num">2</td>`, `class="pill sev-critical">1 critical`} {
		if !strings.Contains(app, want) {
			t.Errorf("app row lacks %q:\n%s", want, app)
		}
	}
	if strings.Contains(app, "sev-high") {
		t.Errorf("the row shows more than the worst severity:\n%s", app)
	}
	if bare := rowFor(t, body, "bare"); !strings.Contains(bare, "not scanned") || strings.Contains(bare, "no findings") {
		t.Errorf("an unscanned image must say so and never look clean:\n%s", bare)
	}
}

func TestUI_RepositoryRowsIgnoreAScanTheClientAttached(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "one")
	if _, err := e.ev.Attach(t.Context(), evidence.AttachParams{
		Subject: d, Type: model.EvidenceScan, MediaType: "application/json",
		Body: strings.NewReader(`{"clean":true}`), Size: 14, Actor: "user:ci",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if row := rowFor(t, e.get("alice", "/ui/r/team-a").body, "app"); !strings.Contains(row, "not scanned") {
		t.Errorf("a client's scan made the row look scanned (ADR 29):\n%s", row)
	}
	if row := rowFor(t, e.get("alice", "/ui/r/team-a/-/package?name=app").body, "1.0"); !strings.Contains(row, "not scanned") {
		t.Errorf("a client's scan made the tag look scanned (ADR 29):\n%s", row)
	}
}

func TestUI_RepositoryFilterAndSortNarrowAndOrderTheRows(t *testing.T) {
	e := newEnv(t)
	e.publishNPM("npm", "alpha", "1.0.0", "latest")
	time.Sleep(10 * time.Millisecond)
	e.publishNPM("npm", "beta", "1.0.0", "latest")
	time.Sleep(10 * time.Millisecond)
	e.publishNPM("npm", "gamma-alpha", "1.0.0", "latest")
	names := func(body string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`<tr><td><a class="mono" href="[^"]*">([^<]+)</a>`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"ByNameByDefault", "", []string{"alpha", "beta", "gamma-alpha"}},
		{"FilterIsACaseInsensitiveSubstring", "?q=ALPHA", []string{"alpha", "gamma-alpha"}},
		{"NewestPublishFirst", "?sort=published", []string{"gamma-alpha", "beta", "alpha"}},
		{"FilterAndSortTogether", "?q=alpha&sort=published", []string{"gamma-alpha", "alpha"}},
		{"AnUnknownSortIsDropped", "?sort=drop%20table", []string{"alpha", "beta", "gamma-alpha"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := e.get("alice", "/ui/r/npm"+tc.query)
			if r.status != http.StatusOK {
				t.Fatalf("status %d", r.status)
			}
			if got := names(r.body); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("rows = %v, want %v", got, tc.want)
			}
		})
	}
	if r := e.get("alice", "/ui/r/npm?q=zzz"); !strings.Contains(r.body, "No package matches this filter") || !strings.Contains(r.body, "filter-form") {
		t.Errorf("a filter with no match should say so and keep the form:\n%s", r.body)
	}
	if r := e.get("alice", "/ui/r/npm?q=alpha", "HX-Request", "true"); strings.Contains(r.body, "<html") || strings.Contains(r.body, "filter-form") {
		t.Errorf("htmx got more than the results:\n%s", r.body)
	}
}

func TestUI_NPMRowsUseTheLatestDistTagAndSemverOrder(t *testing.T) {
	e := newEnv(t)
	e.publishNPM("npm", "pkg", "1.9.0", "latest")
	e.publishNPM("npm", "pkg", "1.10.0", "next")
	e.publishNPM("npm", "pkg", "2.0.0-rc.1", "rc")
	row := rowFor(t, e.get("alice", "/ui/r/npm").body, "pkg")
	if !strings.Contains(row, ">1.9.0<") || !strings.Contains(row, `<td class="num">3</td>`) {
		t.Errorf("the row's latest is not the latest dist-tag's version:\n%s", row)
	}
	page := e.get("alice", "/ui/r/npm/-/package?name=pkg").body
	order := regexp.MustCompile(`<td><span class="mono">([^<]+)</span>`).FindAllStringSubmatch(page, -1)
	var got []string
	for _, m := range order {
		got = append(got, m[1])
	}
	if want := "[2.0.0-rc.1 1.10.0 1.9.0]"; fmt.Sprint(got) != want {
		t.Errorf("versions are listed %v, want semver order, highest first: %s", got, want)
	}
}

func TestUI_PyPIRowsUseTheHighestPEP440VersionAndOrder(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.9", "1.10", "1.10rc1"} {
		if _, err := e.reg.Publish(t.Context(), registry.PublishParams{
			Repo: "py", Package: "demo", Version: v, Filename: "demo-" + v + ".tar.gz",
			Body: strings.NewReader("sdist " + v), Size: int64(len("sdist " + v)),
		}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if row := rowFor(t, e.get("alice", "/ui/r/py").body, "demo"); !strings.Contains(row, ">1.10<") {
		t.Errorf("the row's latest is not the highest version:\n%s", row)
	}
	page := e.get("alice", "/ui/r/py/-/package?name=demo").body
	var got []string
	for _, m := range regexp.MustCompile(`<td><span class="mono">([^<]+)</span>`).FindAllStringSubmatch(page, -1) {
		got = append(got, m[1])
	}
	if want := "[1.10 1.10rc1 1.9]"; fmt.Sprint(got) != want {
		t.Errorf("versions are listed %v, want PEP 440 order, highest first: %s", got, want)
	}
}

func TestUI_TagRowsShowStageSeverityOwnersSizeAndSharedDigests(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "one")
	if d2 := e.push("team-a", "app", "stable", "one"); d2 != d {
		t.Fatalf("two tags of one manifest have two digests")
	}
	e.push("team-a", "app", "2.0", "two")
	e.promote(t, d, "team-a", "dev")
	e.promote(t, d, "team-a", "prod")
	e.sindriScan(d, "linux/amd64", map[string]int{"HIGH": 3, "LOW": 1})
	e.sindriScan(d, "linux/arm64", map[string]int{"HIGH": 1})

	body := e.get("alice", "/ui/r/team-a/-/package?name=app").body
	one := rowFor(t, body, "1.0")
	for _, want := range []string{
		`class="pill approve">prod`, `class="pill sev-high">4 high`, `class="pill sev-low">1 low`,
		"shared digest", "repository:team-a", "just now",
	} {
		if !strings.Contains(one, want) {
			t.Errorf("tag row lacks %q:\n%s", want, one)
		}
	}
	if size := regexp.MustCompile(`<td class="num col-size">([^<]+)</td>`).FindStringSubmatch(one); size == nil || size[1] == "-" || size[1] == "0 B" {
		t.Errorf("tag row has no size: %v", size)
	}
	if row := rowFor(t, body, "2.0"); strings.Contains(row, "shared digest") || !strings.Contains(row, "not promoted") || !strings.Contains(row, "not scanned") {
		t.Errorf("an unpromoted, unscanned, unshared tag row is wrong:\n%s", row)
	}
}

// A subject placed in two teams' repositories shows each team only its own
// owner and its own stage (I2, security rule 8).
func TestUI_RowsShowOnlyWhatTheCallerMayRead(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "shared")
	e.push("team-b", "app", "1.0", "shared")
	e.promote(t, d, "team-b", "prod")

	for _, path := range []string{"/ui/r/team-a/-/package?name=app", "/ui/r/team-a"} {
		r := e.get("alice", path)
		if strings.Contains(r.body, "team-b") {
			t.Errorf("alice's %s names team-b, which she may not read", path)
		}
	}
	tag := rowFor(t, e.get("alice", "/ui/r/team-a/-/package?name=app").body, "1.0")
	if !strings.Contains(tag, "not promoted") || strings.Contains(tag, "approve") {
		t.Errorf("team-b's approval leaked into team-a's row:\n%s", tag)
	}
	if !strings.Contains(tag, "repository:team-a") {
		t.Errorf("alice does not see her own owner:\n%s", tag)
	}
	if r := e.get("alice", "/ui/r/team-b"); r.status != http.StatusNotFound {
		t.Errorf("alice reads team-b's repository page: %d", r.status)
	}
}

func TestUI_ProxyRowsCarryNoStageOrSeverity(t *testing.T) {
	e := newEnv(t)
	info, err := e.cas.PutBytes(t.Context(), []byte("cached tarball"))
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if _, err := e.reg.PlaceCachedFile(t.Context(), registry.PlaceCachedFileParams{
		Repo: "npmjs", Package: "left-pad", Version: "1.0.0", Filename: "left-pad-1.0.0.tgz", Blob: info,
	}); err != nil {
		t.Fatalf("PlaceCachedFile: %v", err)
	}
	for _, path := range []string{"/ui/r/npmjs", "/ui/r/npmjs/-/package?name=left-pad"} {
		body := e.get("bob", path).body
		for _, banned := range []string{"not promoted", "not scanned", "no findings", "Worst severity", "Stage", "Findings", `class="pill approve"`} {
			if strings.Contains(body, banned) {
				t.Errorf("%s shows %q for proxy content", path, banned)
			}
		}
	}
	if !strings.Contains(e.get("bob", "/ui/r/npmjs").body, "left-pad") {
		t.Error("the proxy page lost its row")
	}
}

func TestUI_RepositoryHeaderFacts(t *testing.T) {
	e := newEnv(t)
	hosted := e.get("alice", "/ui/r/team-a").body
	for _, want := range []string{"Managed by", "api", "Lifecycle stages", `<span class="pill">dev</span>`, `<span class="pill">prod</span>`} {
		if !strings.Contains(hosted, want) {
			t.Errorf("hosted repository page lacks %q", want)
		}
	}
	if strings.Contains(hosted, "Cached entries") || strings.Contains(hosted, "Upstream") {
		t.Error("a hosted repository shows proxy facts")
	}

	// A restricted repository lists only the stages it may promote into.
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: "gated", Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI, LifecycleStages: []string{"dev"},
	}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	gated := e.get("admin", "/ui/r/gated").body
	if !strings.Contains(gated, `<span class="pill">dev</span>`) || strings.Contains(gated, `<span class="pill">prod</span>`) {
		t.Errorf("gated repository does not list just its own stage:\n%s", gated)
	}

	proxy := e.get("bob", "/ui/r/npmjs").body
	for _, want := range []string{"Upstream", "Cached entries", "Last successful fetch", "never", "content appears on first pull"} {
		if !strings.Contains(proxy, want) {
			t.Errorf("an empty proxy page lacks %q", want)
		}
	}
	repoID := ""
	if err := e.pool.QueryRow(t.Context(), `SELECT id::text FROM repositories WHERE name = 'npmjs'`).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ key, status string }{{"a", "hit"}, {"b", "hit"}, {"c", "miss"}} {
		if _, err := e.pool.Exec(t.Context(), `INSERT INTO remote_cache (repo_id, cache_key, status, fetched_at) VALUES ($1::uuid, $2, $3, now() - interval '3 hours')`, repoID, row.key, row.status); err != nil {
			t.Fatalf("seeding the cache: %v", err)
		}
	}
	proxy = e.get("bob", "/ui/r/npmjs").body
	if !strings.Contains(proxy, "<dt>Cached entries</dt><dd>2</dd>") || !strings.Contains(proxy, "3 hours ago") {
		t.Errorf("proxy facts do not count the two hits and the last fetch (a miss is not content):\n%s", proxy)
	}
}

func TestUI_EmptyRepositoriesShowThePublishCommandOrAnEmptyProxyNote(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		repo string
		want []string
	}{
		{"team-a", []string{"Nothing published here yet", "docker push"}},
		{"py", []string{"Nothing published here yet", "twine upload", "/pypi/py/upload"}},
		{"npm", []string{"Nothing published here yet", "npm publish --registry"}},
		{"hub", []string{"content appears on first pull"}},
	}
	for _, tc := range tests {
		t.Run(tc.repo, func(t *testing.T) {
			body := e.get("admin", "/ui/r/"+tc.repo).body
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("empty %s page lacks %q", tc.repo, want)
				}
			}
			if tc.repo == "hub" && (strings.Contains(body, "docker push") || strings.Contains(body, "Nothing published")) {
				t.Error("an empty proxy offers a publish command")
			}
		})
	}
}

func TestUI_TagLoadMoreReturnsOnlyRows(t *testing.T) {
	e := newEnv(t)
	for i := range pageSize + 5 {
		e.push("team-a", "app", fmt.Sprintf("t%03d", i), fmt.Sprintf("seed-%d", i))
	}
	first := e.get("alice", "/ui/r/team-a/-/package?name=app")
	more := regexp.MustCompile(`hx-get="([^"]+)"`).FindStringSubmatch(first.body)
	if more == nil {
		t.Fatal("a package with more than one page of tags has no load-more control")
	}
	next := e.get("alice", strings.ReplaceAll(more[1], "&amp;", "&"), "HX-Request", "true")
	if strings.Contains(next.body, "<html") || strings.Contains(next.body, "<thead") {
		t.Fatal("load more returned a whole page, not rows")
	}
	if got := strings.Count(next.body, `<td><span class="mono">t`); got != 5 {
		t.Fatalf("load more returned %d rows, want the 5 left", got)
	}
}

// The filter form posts to the bare repository URL. htmx appends the form's
// values to a GET URL that already has a query, and Go reads the first value
// of a repeated key, so an action carrying ?sort= would let a stale sort beat
// the select.
func TestUI_PackageFilterFormHasNoQueryInItsAction(t *testing.T) {
	e := newEnv(t)
	e.publishNPM("npm", "alpha", "1.0.0", "latest")
	body := e.get("alice", "/ui/r/npm?sort=published&q=alp").body
	form := regexp.MustCompile(`(?s)<form class="filter-form"[^>]*>`).FindString(body)
	if form == "" {
		t.Fatalf("no filter form:\n%s", body)
	}
	for _, want := range []string{`action="/ui/r/npm"`, `hx-get="/ui/r/npm"`} {
		if !strings.Contains(form, want) {
			t.Errorf("filter form lacks %s: %s", want, form)
		}
	}
	if !strings.Contains(body, `<option value="published" selected>`) {
		t.Error("the sort in the URL is not the selected option")
	}
}

// An index is scanned only when every platform its newest scan lists has a
// scan, as the severity gate reads it (ADR 30). One of two, clean, must say
// "not scanned" everywhere, never "no findings".
func TestUI_APartlyScannedIndexNeverReadsAsScannedOrClean(t *testing.T) {
	e := newEnv(t)
	idx := e.pushIndex("team-a", "app", "multi", "partial")
	both := []string{"linux/amd64", "linux/arm64"}
	e.indexScan(idx, "linux/amd64", both, nil)

	verdict := func() string {
		body := e.get("alice", artifactPath("team-a", "app", idx, "&tag=multi")).body
		start := strings.Index(body, `<div class="pills verdict"`)
		if start < 0 {
			t.Fatalf("no verdict strip:\n%s", body)
		}
		return body[start : start+strings.Index(body[start:], "</div>")]
	}
	check := func(label string, wantScanned bool) {
		t.Helper()
		views := map[string]string{
			"verdict strip": verdict(),
			"tag row":       rowFor(t, e.get("alice", "/ui/r/team-a/-/package?name=app").body, "multi"),
		}
		for name, v := range views {
			if wantScanned != strings.Contains(v, "no findings") || wantScanned == strings.Contains(v, "not scanned") {
				t.Errorf("%s: the %s reads wrongly (scanned want %v):\n%s", label, name, wantScanned, v)
			}
		}
	}
	check("one of two platforms scanned", false)
	e.indexScan(idx, "linux/arm64", both, nil)
	check("both platforms scanned", true)
}

func TestUI_PackageRowOfAPartlyScannedIndexIsNotScanned(t *testing.T) {
	e := newEnv(t)
	idx := e.pushIndex("team-a", "solo", "multi", "solo")
	// Only the index is current for the image: drop the child tags.
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM tags WHERE name <> 'multi' AND package_id IN (SELECT id FROM packages WHERE normalized_name = 'solo')`); err != nil {
		t.Fatalf("dropping child tags: %v", err)
	}
	both := []string{"linux/amd64", "linux/arm64"}
	e.indexScan(idx, "linux/amd64", both, nil)
	if row := rowFor(t, e.get("alice", "/ui/r/team-a").body, "solo"); !strings.Contains(row, "not scanned") || strings.Contains(row, "no findings") {
		t.Errorf("the repository row of a half-scanned index must not read clean:\n%s", row)
	}
	e.indexScan(idx, "linux/arm64", both, nil)
	if row := rowFor(t, e.get("alice", "/ui/r/team-a").body, "solo"); !strings.Contains(row, "no findings") {
		t.Errorf("a fully scanned clean index should read no findings:\n%s", row)
	}
}

func TestUI_RowsOfAPartlyScannedImageReadNotScanned(t *testing.T) {
	e := newEnv(t)
	d1 := e.push("team-a", "app", "1.0", "scanned")
	e.push("team-a", "app", "2.0", "unscanned")
	e.sindriScan(d1, "", nil)

	if row := rowFor(t, e.get("alice", "/ui/r/team-a").body, "app"); !strings.Contains(row, "not scanned") || strings.Contains(row, "no findings") {
		t.Errorf("one clean tag and one unscanned tag must read not scanned:\n%s", row)
	}
	body := e.get("alice", "/ui/r/team-a/-/package?name=app").body
	if row := rowFor(t, body, "1.0"); !strings.Contains(row, "no findings") {
		t.Errorf("the scanned tag lost its own result:\n%s", row)
	}
	if row := rowFor(t, body, "2.0"); !strings.Contains(row, "not scanned") {
		t.Errorf("the unscanned tag:\n%s", row)
	}
}

func TestUI_RowsSumTheNewestScanPerPlatformOnly(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "rescanned")
	e.sindriScan(d, "linux/amd64", map[string]int{"HIGH": 5}) // superseded
	e.sindriScan(d, "linux/amd64", nil)
	e.sindriScan(d, "linux/arm64", map[string]int{"LOW": 2})

	for _, path := range []string{"/ui/r/team-a", "/ui/r/team-a/-/package?name=app"} {
		body := e.get("alice", path).body
		name := "app"
		if strings.Contains(path, "package") {
			name = "1.0"
		}
		row := rowFor(t, body, name)
		if strings.Contains(row, "sev-high") || strings.Contains(row, "5 high") {
			t.Errorf("%s: the older scan's findings still count:\n%s", path, row)
		}
		if !strings.Contains(row, "2 low") {
			t.Errorf("%s: the other platform's findings are missing:\n%s", path, row)
		}
	}
	if row := rowFor(t, e.get("alice", "/ui/r/team-a").body, "app"); strings.Contains(row, "sev-high") {
		t.Errorf("repository row: %s", row)
	}
}

func TestUI_RowsReadARevokedDigestAsTheVerdictStripDoes(t *testing.T) {
	e := newEnv(t)
	revoke := func(d model.Digest, stage string) {
		t.Helper()
		if _, err := e.lc.Revoke(t.Context(), lifecycle.ChangeParams{Digest: d, Repository: "team-a", Stage: stage, Actor: "user:admin", Reason: "bad"}); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
	}
	only := e.push("team-a", "app", "dev-only", "dev-only")
	e.promote(t, only, "team-a", "dev")
	revoke(only, "dev")
	both := e.push("team-a", "app", "both", "both")
	e.promote(t, both, "team-a", "dev")
	e.promote(t, both, "team-a", "prod")
	revoke(both, "prod")
	kept := e.push("team-a", "app", "kept", "kept")
	e.promote(t, kept, "team-a", "dev")
	e.promote(t, kept, "team-a", "prod")
	revoke(kept, "dev")

	body := e.get("alice", "/ui/r/team-a/-/package?name=app").body
	for tag, want := range map[string]string{
		"dev-only": `class="pill revoke">revoked from dev`,
		"both":     `class="pill revoke">revoked from prod`,
		"kept":     `class="pill approve">prod`,
	} {
		if row := rowFor(t, body, tag); !strings.Contains(row, want) || (tag != "kept" && strings.Contains(row, "not promoted")) {
			t.Errorf("tag %s: want %q:\n%s", tag, want, row)
		}
	}
	// The strip on the artifact page agrees with the row.
	strip := e.get("alice", artifactPath("team-a", "app", only, "&tag=dev-only")).body
	if !strings.Contains(strip, `class="pill revoke">revoked from dev`) {
		t.Errorf("the verdict strip disagrees with the row:\n%s", strip)
	}
}

// A digest cached in a proxy that is also pushed to a hosted repository the
// reader cannot read must give the proxy page nothing from that repository:
// no latest tag, no publish time, no influence on the sort (I2).
func TestUI_ProxyRowsTakeNothingFromAHostedRepositoryHoldingTheSameDigest(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-b", "secret", "internal-release", "same-bytes")
	if r := e.get("alice", "/ui/r/team-b"); r.status != http.StatusNotFound {
		t.Fatalf("alice must not read team-b: %d", r.status)
	}
	if _, err := e.reg.PlaceCachedManifest(t.Context(), "hub", "library/alpine", "3.20", ocispec.MediaTypeImageManifest, mustManifestBody(t, e, "team-b", "same-bytes")); err != nil {
		t.Fatalf("PlaceCachedManifest: %v", err)
	}
	_ = d
	for _, path := range []string{"/ui/r/hub", "/ui/r/hub?sort=published"} {
		body := e.get("alice", path).body
		row := rowFor(t, body, "library/alpine")
		if strings.Contains(row, "3.20") || strings.Contains(row, "just now") || strings.Contains(row, "<time") {
			t.Errorf("%s: the proxy row carries facts of a hosted repository:\n%s", path, row)
		}
		if strings.Contains(body, "team-b") || strings.Contains(body, "internal-release") {
			t.Errorf("%s names the hidden repository or its tag", path)
		}
	}
}

// A scan written before index scans listed their platforms is refused by the
// gate ("does not list the platforms of its index"), so the page must not read
// the digest as scanned or clean, in the strip or the row.
func TestUI_ALegacyPerPlatformIndexScanNeverReadsAsScanned(t *testing.T) {
	e := newEnv(t)
	idx := e.pushIndex("team-a", "legacy", "multi", "legacy")
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM tags WHERE name <> 'multi' AND package_id IN (SELECT id FROM packages WHERE normalized_name = 'legacy')`); err != nil {
		t.Fatalf("dropping child tags: %v", err)
	}
	for _, arch := range []string{"linux/amd64", "linux/arm64"} {
		if _, err := e.pool.Exec(t.Context(), `
			INSERT INTO evidence (subject_digest, type, media_type, blob_digest, metadata, created_by)
			VALUES ($1, 'scan', 'application/json', $1, $2::jsonb, 'system:scanner')`, idx.String(),
			fmt.Sprintf(`{"producer":"sindri","platform":%q,"vulnerabilities":[],"severity_counts":{}}`, arch)); err != nil {
			t.Fatalf("recording a legacy scan: %v", err)
		}
	}
	body := e.get("alice", artifactPath("team-a", "legacy", idx, "&tag=multi")).body
	start := strings.Index(body, `<div class="pills verdict"`)
	if start < 0 {
		t.Fatalf("no verdict strip:\n%s", body)
	}
	strip := body[start : start+strings.Index(body[start:], "</div>")]
	if !strings.Contains(strip, "not scanned") || strings.Contains(strip, "no findings") {
		t.Errorf("the verdict strip reads a refused scan as scanned:\n%s", strip)
	}
	for _, c := range []struct{ path, name string }{{"/ui/r/team-a", "legacy"}, {"/ui/r/team-a/-/package?name=legacy", "multi"}} {
		if row := rowFor(t, e.get("alice", c.path).body, c.name); !strings.Contains(row, "not scanned") || strings.Contains(row, "no findings") {
			t.Errorf("%s reads a refused scan as scanned:\n%s", c.path, row)
		}
	}
}
