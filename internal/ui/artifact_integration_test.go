//go:build integration

package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
	"github.com/spj/sindri/internal/registry"
)

func (e *env) createRepo(t *testing.T, name string, read ...string) {
	t.Helper()
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI, Access: model.Access{Read: read},
	}); err != nil {
		t.Fatalf("CreateRepository %s: %v", name, err)
	}
}

func artifactPath(repo, image string, d model.Digest, extra string) string {
	p := "/ui/r/" + repo + "/-/artifact?digest=" + d.String()
	if image != "" {
		p += "&image=" + image
	}
	return p + extra
}

// "Also placed in" lists the other repositories that place the digest and that
// the caller may read, and nothing else. A repository the caller cannot read is
// neither named nor counted, so the line reads the same whether or not it exists.
func TestUI_ArtifactAlsoPlacedInNeverNamesAnUnreadableRepository(t *testing.T) {
	e := newEnv(t)
	e.createRepo(t, "team-a-mirror", "team:a")
	d := e.push("team-a", "app", "1.0", "shared")
	for _, repo := range []string{"team-a-mirror", "team-b"} {
		if d2 := e.push(repo, "app", "1.0", "shared"); d2 != d {
			t.Fatalf("the same manifest has two digests: %s %s", d, d2)
		}
	}

	t.Run("AliceSeesOnlyTheMirrorSheMayRead", func(t *testing.T) {
		r := e.get("alice", artifactPath("team-a", "app", d, ""))
		if r.status != http.StatusOK {
			t.Fatalf("GET: %d\n%s", r.status, r.body)
		}
		if !strings.Contains(r.body, "Also placed in") || !strings.Contains(r.body, `href="/ui/r/team-a-mirror"`) {
			t.Errorf("alice's page does not list the mirror she may read:\n%s", r.body)
		}
		if strings.Contains(r.body, "team-b") {
			t.Errorf("alice's page names team-b, which she may not read:\n%s", r.body)
		}
	})
	t.Run("AdminSeesBoth", func(t *testing.T) {
		r := e.get("admin", artifactPath("team-a", "app", d, ""))
		for _, want := range []string{`href="/ui/r/team-a-mirror"`, `href="/ui/r/team-b"`} {
			if !strings.Contains(r.body, want) {
				t.Errorf("admin's page lacks %s", want)
			}
		}
	})
	t.Run("ADigestPlacedOnlyHereHasNoLine", func(t *testing.T) {
		only := e.push("team-a", "solo", "1.0", "only-a")
		r := e.get("alice", artifactPath("team-a", "solo", only, ""))
		if strings.Contains(r.body, "Also placed in") {
			t.Errorf("a digest placed in one repository claims another:\n%s", r.body)
		}
	})
	t.Run("BobCannotUseTheLineToReachTeamA", func(t *testing.T) {
		if r := e.get("bob", artifactPath("team-a", "app", d, "")); r.status != http.StatusNotFound {
			t.Errorf("bob reads team-a's artifact page: %d", r.status)
		}
		r := e.get("bob", artifactPath("team-b", "app", d, ""))
		if strings.Contains(r.body, "team-a") {
			t.Errorf("bob's page names a team-a repository:\n%s", r.body)
		}
	})
}

// The cap on "Also placed in" applies to the repositories the caller may read.
// Unreadable repositories that sort first cannot push readable ones out of the
// list, and are neither named nor counted.
func TestUI_ArtifactAlsoPlacedInCapIgnoresUnreadableRepositories(t *testing.T) {
	old := alsoPlacedLimit
	alsoPlacedLimit = 3
	t.Cleanup(func() { alsoPlacedLimit = old })

	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "wide")
	for _, name := range []string{"aa-1", "aa-2", "aa-3", "aa-4", "aa-5", "aa-6"} {
		e.createRepo(t, name, "team:b")
		e.push(name, "app", "1.0", "wide")
	}
	readable := []string{"zz-1", "zz-2", "zz-3"}
	for _, name := range readable {
		e.createRepo(t, name, "team:a")
		e.push(name, "app", "1.0", "wide")
	}

	r := e.get("alice", artifactPath("team-a", "app", d, ""))
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	for _, name := range readable {
		if !strings.Contains(r.body, `href="/ui/r/`+name+`"`) {
			t.Errorf("readable %s was pushed out by unreadable repositories", name)
		}
	}
	if strings.Contains(r.body, "aa-") || strings.Contains(r.body, "team-b") || strings.Contains(r.body, "more not shown") {
		t.Errorf("the page names or hints at unreadable repositories:\n%s", r.body)
	}

	// With a fourth readable repository the cap bites, and says so.
	e.createRepo(t, "zz-4", "team:a")
	e.push("zz-4", "app", "1.0", "wide")
	r = e.get("alice", artifactPath("team-a", "app", d, ""))
	i := strings.Index(r.body, "<dt>Also placed in</dt>")
	if i < 0 {
		t.Fatalf("no Also placed in line:\n%s", r.body)
	}
	line := r.body[i : i+strings.Index(r.body[i:], "</dd>")]
	if !strings.Contains(line, "more not shown") || strings.Contains(line, "zz-4") || strings.Contains(r.body, "aa-") {
		t.Errorf("a capped list must say so and stay readable-only: %s", line)
	}
}

func TestUI_ArtifactTitleAndNamesPointHere(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "named")
	if _, err := e.reg.PutManifest(t.Context(), "team-a", "app", "latest", ocispec.MediaTypeImageManifest, mustManifestBody(t, e, "team-a", "named"), model.Provenance{SourceType: "oci-push"}); err != nil {
		t.Fatalf("tagging latest: %v", err)
	}
	r := e.get("alice", artifactPath("team-a", "app", d, "&tag=latest"))
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	for _, want := range []string{
		`>app:latest <span class="sub mono">` + shortDigest(d.String()) + `</span></h1>`,
		"<title>app:latest " + shortDigest(d.String()) + " · Sindri</title>",
		">app:1.0</a>", ">app:latest</a>",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("artifact page lacks %q\n%s", want, r.body)
		}
	}
	// A tag that does not point here is not a name.
	other := e.push("team-a", "app", "2.0", "someone-else")
	r = e.get("alice", artifactPath("team-a", "app", other, "&tag=latest"))
	if strings.Contains(r.body, "app:latest") {
		t.Errorf("a stale tag in the URL became the title:\n%s", r.body)
	}
}

// mustManifestBody is the body e.push places for seed, so a second tag can name
// the same digest.
func mustManifestBody(t *testing.T, e *env, repo, seed string) []byte {
	t.Helper()
	up, err := e.reg.CommitBlobUpload(t.Context(), repo, e.upload([]byte("layer "+seed)), model.Digest{})
	if err != nil {
		t.Fatalf("CommitBlobUpload: %v", err)
	}
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig, Digest: digestOf(up.Digest), Size: up.Size,
	}}
	man.SchemaVersion = 2
	body, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestUI_ArtifactVerdictStrip(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "verdict")
	path := artifactPath("team-a", "app", d, "")
	strip := func(t *testing.T) string {
		t.Helper()
		r := e.get("alice", path)
		start := strings.Index(r.body, `<div class="pills verdict"`)
		if start < 0 {
			t.Fatalf("no verdict strip:\n%s", r.body)
		}
		return r.body[start : start+strings.Index(r.body[start:], "</div>")]
	}

	if s := strip(t); !strings.Contains(s, "not promoted") || !strings.Contains(s, "not scanned") || strings.Contains(s, "no findings") {
		t.Errorf("a new push must read not promoted and not scanned, never clean: %s", s)
	}
	e.sindriScan(d, "", map[string]int{"LOW": 1})
	e.promote(t, d, "team-a", "dev")
	if s := strip(t); !strings.Contains(s, `class="pill approve">dev`) || !strings.Contains(s, "1 low") || !strings.Contains(s, "scanned") || strings.Contains(s, "new findings") {
		t.Errorf("approved with a quiet scan: %s", s)
	}
	e.sindriScan(d, "", map[string]int{"HIGH": 2, "LOW": 1})
	if s := strip(t); !strings.Contains(s, "2 high") || !strings.Contains(s, "new findings since approval") {
		t.Errorf("a worse scan after the approval: %s", s)
	}
	if _, err := e.lc.Revoke(t.Context(), lifecycle.ChangeParams{Digest: d, Repository: "team-a", Stage: "dev", Actor: "user:admin", Reason: "bad"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if s := strip(t); !strings.Contains(s, `class="pill revoke">revoked from dev`) || strings.Contains(s, "new findings") {
		t.Errorf("revoked: %s", s)
	}
}

func TestUI_UpstreamArtifactShowsNoVerdictStrip(t *testing.T) {
	e := newEnv(t)
	idx := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	m, err := e.reg.PlaceCachedManifest(t.Context(), "hub", "library/alpine", "3.20", "application/vnd.oci.image.index.v1+json", idx)
	if err != nil {
		t.Fatalf("PlaceCachedManifest: %v", err)
	}
	r := e.get("alice", artifactPath("hub", "library%2Falpine", m.Digest, "&tag=3.20"))
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	for _, banned := range []string{`verdict`, "not promoted", "not scanned", `id="lifecycle"`, `id="vulnerabilities"`} {
		if strings.Contains(r.body, banned) {
			t.Errorf("an upstream artifact page shows %q", banned)
		}
	}
	if !strings.Contains(r.body, "Fetched from an upstream") {
		t.Error("the upstream callout is gone")
	}
}

func TestUI_IndexArtifactShowsAPlatformTableWithEachChildsVerdict(t *testing.T) {
	e := newEnv(t)
	amd := e.push("team-a", "app", "amd", "amd64-child")
	arm := e.push("team-a", "app", "arm", "arm64-child")
	e.promote(t, amd, "team-a", "dev")
	e.sindriScan(amd, "", map[string]int{"CRITICAL": 3})
	e.sindriScan(arm, "", nil)

	desc := func(d model.Digest, arch string) ocispec.Descriptor {
		info, err := e.reg.StatBlob(t.Context(), "team-a", d)
		if err != nil {
			t.Fatalf("StatBlob: %v", err)
		}
		return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digestOf(d), Size: info.Size,
			Platform: &ocispec.Platform{OS: "linux", Architecture: arch}}
	}
	index := ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{desc(amd, "amd64"), desc(arm, "arm64")}}
	index.SchemaVersion = 2
	body, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := e.reg.PutManifest(t.Context(), "team-a", "app", "multi", ocispec.MediaTypeImageIndex, body, model.Provenance{SourceType: "oci-push"})
	if err != nil {
		t.Fatalf("PutManifest index: %v", err)
	}

	r := e.get("alice", artifactPath("team-a", "app", idx.Digest, "&tag=multi"))
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	amdRow := rowCell(t, r.body, "linux/amd64")
	armRow := rowCell(t, r.body, "linux/arm64")
	if !strings.Contains(amdRow, `pill approve">dev`) || !strings.Contains(amdRow, "3 critical") {
		t.Errorf("amd64 row lacks its own verdict: %s", amdRow)
	}
	if !strings.Contains(armRow, "not promoted") || !strings.Contains(armRow, "no findings") {
		t.Errorf("arm64 row: %s", armRow)
	}
	if strings.Contains(r.body, `class="bar-track"`) {
		t.Error("an index shows a layer size bar")
	}
}

func rowCell(t *testing.T, body, platform string) string {
	t.Helper()
	i := strings.Index(body, `<td class="mono">`+platform+`</td>`)
	if i < 0 {
		t.Fatalf("no row for %s in:\n%s", platform, body)
	}
	return body[i : i+strings.Index(body[i:], "</tr>")]
}

func TestUI_ImageArtifactShowsLayersWithBuildStepsAndSizeBars(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	put := func(b []byte) ocispec.Descriptor {
		up, err := e.reg.CommitBlobUpload(ctx, "team-a", e.upload(b), model.Digest{})
		if err != nil {
			t.Fatalf("CommitBlobUpload: %v", err)
		}
		return ocispec.Descriptor{Digest: digestOf(up.Digest), Size: up.Size}
	}
	cfgBody, err := json.Marshal(ocispec.Image{History: []ocispec.History{
		{CreatedBy: "ADD rootfs.tar /"},
		{CreatedBy: "ENV PATH=/bin", EmptyLayer: true},
		{CreatedBy: "RUN make   install"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := put(cfgBody)
	cfg.MediaType = ocispec.MediaTypeImageConfig
	small, big := put([]byte("small layer")), put([]byte(strings.Repeat("big layer ", 100)))
	small.MediaType, big.MediaType = ocispec.MediaTypeImageLayer, ocispec.MediaTypeImageLayer
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{small, big}}
	man.SchemaVersion = 2
	body, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	placed, err := e.reg.PutManifest(ctx, "team-a", "app", "1.0", ocispec.MediaTypeImageManifest, body, model.Provenance{SourceType: "oci-push"})
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	r := e.get("alice", artifactPath("team-a", "app", placed.Digest, ""))
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	for _, want := range []string{
		`<span class="build-step mono">ADD rootfs.tar /</span>`, `<span class="build-step mono">RUN make install</span>`,
		`class="bar" data-w="10"`, `class="bar" data-w="1"`, `<span class="pill">config</span>`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("artifact page lacks %q\n%s", want, r.body)
		}
	}
	if strings.Contains(r.body, "ENV PATH") {
		t.Error("an empty-layer history entry became a layer's build step")
	}
}

// A digest with no name in the repository has no title: the image in the link
// is the caller's claim, and a crafted link must not give a digest a name.
func TestUI_ArtifactTitleNeverTakesAnImageNameFromTheURL(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "nameless")
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM tags WHERE target_digest = $1`, d.String()); err != nil {
		t.Fatalf("dropping the tag: %v", err)
	}
	r := e.get("alice", artifactPath("team-a", "trusted-vendor-base", d, ""))
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	if strings.Contains(r.body, "<title>trusted-vendor-base") || strings.Contains(r.body, `class="page-title">trusted-vendor-base`) || strings.Contains(r.body, `class="sub mono"`) {
		t.Errorf("the URL's image became the page's name:\n%s", r.body)
	}
	if !strings.Contains(r.body, "<title>"+shortDigest(d.String())) {
		t.Errorf("the title is not the digest alone:\n%s", r.body)
	}
}

// cacheHostedBytesInProxy pushes a manifest to the hosted repository team-b,
// promotes it, scans it, and caches the same bytes through the OCI proxy hub,
// as a mirrored base image would be. It returns the proxy page's path.
func cacheHostedBytesInProxy(t *testing.T, e *env) (model.Digest, string) {
	t.Helper()
	d := e.push("team-b", "secret", "internal-release", "same-bytes")
	e.promote(t, d, "team-b", "dev")
	e.sindriScan(d, "", map[string]int{"HIGH": 2})
	m, err := e.reg.PlaceCachedManifest(t.Context(), "hub", "library/alpine", "3.20", ocispec.MediaTypeImageManifest, mustManifestBody(t, e, "team-b", "same-bytes"))
	if err != nil {
		t.Fatalf("PlaceCachedManifest: %v", err)
	}
	if m.Digest != d {
		t.Fatalf("the proxy cached %s, not the hosted digest %s", m.Digest, d)
	}
	return d, artifactPath("hub", "library%2Falpine", d, "&tag=3.20")
}

// identityMarkers are what a page shows only for a subject's identity: none
// of them may appear for proxy content, which has no identity (SIN-122).
var identityMarkers = []string{
	`verdict`, "not promoted", `pill approve`, "2 high", `id="lifecycle"`, `id="vulnerabilities"`,
	`id="evidence"`, `id="provenance"`, "repository:team-b", "oci-push",
}

// A digest cached through a proxy and also published to a hosted repository
// the reader cannot read: the proxy's artifact page shows the upstream notice
// and nothing of the hosted subject, and never names the hosted repository
// (I2, security rules 1 and 8).
func TestUI_ProxyArtifactPageTakesNothingFromAHostedSubjectOfTheSameDigest(t *testing.T) {
	e := newEnv(t)
	_, path := cacheHostedBytesInProxy(t, e)
	if r := e.get("alice", "/ui/r/team-b"); r.status != http.StatusNotFound {
		t.Fatalf("alice must not read team-b: %d", r.status)
	}

	r := e.get("alice", path)
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	if !strings.Contains(r.body, "Fetched from an upstream") {
		t.Error("the upstream notice is gone")
	}
	for _, banned := range identityMarkers {
		if strings.Contains(r.body, banned) {
			t.Errorf("the proxy artifact page shows %q from the hosted subject", banned)
		}
	}
	for _, hidden := range []string{"team-b", "internal-release", "secret"} {
		if strings.Contains(r.body, hidden) {
			t.Errorf("the proxy artifact page names %q of a repository alice cannot read", hidden)
		}
	}
}

// Even a reader of both repositories gets no hosted identity, state, or
// notification history on the proxy page: proxy content is never promoted, and
// what was recorded for the hosted publish belongs to the hosted page.
func TestUI_ProxyArtifactPageShowsNoIdentityEvenToAReaderOfTheHostedRepository(t *testing.T) {
	e := newEnv(t)
	e.withWebhook()
	d, path := cacheHostedBytesInProxy(t, e)
	e.delivery("chat", notify.EventPromoted, d, "delivered", 1, 200, "")

	r := e.get("admin", path)
	if r.status != http.StatusOK {
		t.Fatalf("GET: %d\n%s", r.status, r.body)
	}
	if !strings.Contains(r.body, "Fetched from an upstream") {
		t.Error("the upstream notice is gone")
	}
	for _, banned := range append(identityMarkers, "Notification to") {
		if strings.Contains(r.body, banned) {
			t.Errorf("the proxy artifact page shows %q from the hosted subject", banned)
		}
	}
	if hosted := e.get("admin", artifactPath("team-b", "secret", d, "")).body; !strings.Contains(hosted, `pill approve`) || !strings.Contains(hosted, "Notification to") {
		t.Errorf("the hosted page lost its own identity or deliveries:\n%s", hosted)
	}
}
