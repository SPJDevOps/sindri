//go:build integration

package ui

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
	"github.com/spj/sindri/web"
)

// who maps an X-Test-User header to a principal. GuardUI's own sign-in is
// tested in package auth and end to end in cmd/sindri; here the question is
// what each caller may see, which is the real auth.Authorize.
var who = map[string]auth.Principal{
	"alice": {Subject: "user:alice", Kind: auth.KindUser, Groups: []string{"team:a"}},
	"bob":   {Subject: "user:bob", Kind: auth.KindUser, Groups: []string{"team:b"}},
	"admin": {Subject: "user:admin", Kind: auth.KindUser, Admin: true},
}

type env struct {
	t   *testing.T
	cas *cas.Store
	h   *handler
	reg *registry.Registry
	lc  *lifecycle.Service
	ev  *evidence.Service
	srv http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	database := testsupport.MigratedDB(t)
	store := cas.New(memstore.New(), cas.WithReserver(db.NewBlobReserver(database, "memory")))
	reg := registry.New(database, store, "memory")
	lc := lifecycle.New(database)
	ev := evidence.New(database, store, "memory")
	h, err := newHandler(Options{Registry: reg, Lifecycle: lc, Evidence: ev, SignOut: true})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	// Mounted the way server.Options.UI is: under /ui, behind a principal.
	root := chi.NewRouter()
	root.Mount(Prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := who[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		h.router.ServeHTTP(w, r)
	}))
	e := &env{t: t, cas: store, h: h, reg: reg, lc: lc, ev: ev, srv: root}

	for _, repo := range []registry.CreateRepositoryParams{
		{Name: "team-a", Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI, Access: model.Access{Read: []string{"team:a"}}},
		{Name: "team-b", Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI, Access: model.Access{Read: []string{"team:b"}}},
		{Name: "py", Type: model.RepoTypeHosted, Protocol: model.ProtocolPyPI, Access: model.Access{Read: []string{"team:a"}}},
		{Name: "hub", Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI, UpstreamURL: "https://registry-1.docker.io", TTLSeconds: 60,
			Access: model.Access{Read: []string{auth.Authenticated}}},
		{Name: "all", Type: model.RepoTypeVirtual, Protocol: model.ProtocolOCI, Members: []string{"team-a", "team-b", "hub"},
			Access: model.Access{Read: []string{"team:a"}}},
	} {
		if _, err := reg.CreateRepository(t.Context(), repo); err != nil {
			t.Fatalf("CreateRepository %s: %v", repo.Name, err)
		}
	}
	if _, err := lc.SetStages(t.Context(), []lifecycle.StageSpec{{Name: "dev"}, {Name: "prod"}}); err != nil {
		t.Fatalf("SetStages: %v", err)
	}
	return e
}

type resp struct {
	status int
	header http.Header
	body   string
}

func (e *env) get(user, path string, hdr ...string) resp {
	e.t.Helper()
	req := httptest.NewRequestWithContext(e.t.Context(), http.MethodGet, path, nil)
	req.Header.Set("X-Test-User", user)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	// Unescaped, so assertions read like the page does: html/template writes
	// "+" and quotes as entities.
	return resp{rec.Code, rec.Header(), html.UnescapeString(rec.Body.String())}
}

// push places an image manifest with one layer under tag and returns its
// digest.
func (e *env) push(repo, image, tag, seed string) model.Digest {
	e.t.Helper()
	ctx := e.t.Context()
	layer := []byte("layer " + seed)
	up, err := e.reg.CommitBlobUpload(ctx, repo, e.upload(layer), model.Digest{})
	if err != nil {
		e.t.Fatalf("CommitBlobUpload: %v", err)
	}
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig, Digest: digestOf(up.Digest), Size: up.Size,
	}}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	placed, err := e.reg.PutManifest(ctx, repo, image, tag, ocispec.MediaTypeImageManifest, body,
		model.Provenance{SourceType: "oci-push", SourceRef: "https://git.example.com/" + seed})
	if err != nil {
		e.t.Fatalf("PutManifest: %v", err)
	}
	return placed.Digest
}

// upload stages b in the CAS, ready for CommitBlobUpload.
func (e *env) upload(b []byte) *cas.Upload {
	e.t.Helper()
	up, err := e.cas.NewUpload(e.t.Context())
	if err != nil {
		e.t.Fatalf("NewUpload: %v", err)
	}
	if _, err := up.Append(e.t.Context(), bytes.NewReader(b)); err != nil {
		e.t.Fatalf("Append: %v", err)
	}
	return up
}

func digestOf(d model.Digest) godigest.Digest { return godigest.Digest(d.String()) }

func TestUI_RepositoryListShowsOnlyReadable(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		user        string
		see, notSee []string
	}{
		{"alice", []string{"team-a", "py", "hub", "all"}, []string{"team-b"}},
		{"bob", []string{"team-b", "hub"}, []string{"team-a", "py", "all"}},
		{"admin", []string{"team-a", "team-b", "py", "hub", "all"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.user, func(t *testing.T) {
			r := e.get(tc.user, "/ui/")
			if r.status != http.StatusOK {
				t.Fatalf("GET /ui/: %d", r.status)
			}
			for _, name := range tc.see {
				if !strings.Contains(r.body, `href="/ui/r/`+name+`"`) {
					t.Errorf("%s does not see %s", tc.user, name)
				}
			}
			for _, name := range tc.notSee {
				if strings.Contains(r.body, `/ui/r/`+name+`"`) || strings.Contains(r.body, ">"+name+"<") {
					t.Errorf("%s sees %s, which they may not read", tc.user, name)
				}
			}
		})
	}
}

func TestUI_UnreadableRepositoryPagesAre404(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-b", "app", "1.0", "b")
	for _, path := range []string{
		"/ui/r/team-b",
		"/ui/r/team-b/-/package?name=app",
		"/ui/r/team-b/-/artifact?digest=" + d.String(),
		"/ui/r/no-such-repo",
	} {
		r := e.get("alice", path)
		if r.status != http.StatusNotFound {
			t.Errorf("alice GET %s: %d, want 404", path, r.status)
		}
		if strings.Contains(r.body, d.String()) {
			t.Errorf("alice GET %s shows the digest", path)
		}
	}
	// The same pages work for someone who may read them.
	if r := e.get("bob", "/ui/r/team-b/-/artifact?digest="+d.String()); r.status != http.StatusOK {
		t.Fatalf("bob GET artifact: %d", r.status)
	}
}

// Invariant I2 on the UI: the artifact page is scoped to a repository and
// checks placement. A digest pushed only to team-b is not visible through
// team-a, even to someone who may read both, and even though dedup means its
// bytes are physically present.
func TestUI_ArtifactPageRequiresPlacementInTheRepository(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-b", "app", "1.0", "only-in-b")
	r := e.get("admin", "/ui/r/team-a/-/artifact?digest="+d.String())
	if r.status != http.StatusNotFound {
		t.Fatalf("artifact through a repository it is not placed in: %d, want 404\n%s", r.status, r.body)
	}
	if r := e.get("admin", "/ui/r/team-b/-/artifact?digest="+d.String()); r.status != http.StatusOK {
		t.Fatalf("artifact through its own repository: %d", r.status)
	}
}

func TestUI_ArtifactPageHidesOwnersAndStatesInUnreadableRepositories(t *testing.T) {
	e := newEnv(t)
	// One manifest, pushed to both teams and promoted in each.
	d := e.push("team-a", "app", "1.0", "shared")
	if d2 := e.push("team-b", "app", "1.0", "shared"); d2 != d {
		t.Fatalf("the same manifest has two digests: %s %s", d, d2)
	}
	for _, repo := range []string{"team-a", "team-b"} {
		if _, err := e.lc.Promote(t.Context(), lifecycle.ChangeParams{
			Digest: d, Repository: repo, Stage: "dev", Actor: "user:" + repo, Reason: "test",
		}); err != nil {
			t.Fatalf("Promote in %s: %v", repo, err)
		}
	}

	r := e.get("alice", "/ui/r/team-a/-/artifact?digest="+d.String())
	if r.status != http.StatusOK {
		t.Fatalf("alice GET artifact: %d", r.status)
	}
	if strings.Contains(r.body, "team-b") {
		t.Fatal("alice's artifact page names team-b, which she may not read")
	}
	if !strings.Contains(r.body, "repository:team-a") || !strings.Contains(r.body, "approved") {
		t.Fatal("alice's artifact page lacks her own owner and state")
	}
}

func TestUI_ArtifactPageShowsLifecycleEvidenceAndProvenance(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "story")
	if _, err := e.ev.Attach(t.Context(), evidence.AttachParams{
		Subject: d, Type: model.EvidenceSBOM, MediaType: "application/spdx+json",
		Body: strings.NewReader(`{"spdxVersion":"SPDX-2.3"}`), Size: 26, Actor: "user:ci",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := e.lc.Promote(t.Context(), lifecycle.ChangeParams{
		Digest: d, Repository: "team-a", Stage: "prod", Actor: "user:alice", Reason: "release",
	}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	r := e.get("alice", "/ui/r/team-a/-/artifact?image=app&digest="+d.String())
	for _, want := range []string{
		d.String(), "oci-manifest", "repository:team-a",
		"application/spdx+json", "user:ci", // evidence metadata
		"https://git.example.com/story", // provenance
		"approved", "not promoted",      // prod approved, dev not
		"Config and layers", `"schemaVersion": 2`, // the manifest and its children
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("artifact page lacks %q", want)
		}
	}
	if strings.Contains(r.body, "SPDX-2.3") {
		t.Error("the artifact page shows an evidence document's contents; it must show metadata only")
	}
}

func TestUI_ProxyRepositoryIsMarkedAsUpstreamContent(t *testing.T) {
	e := newEnv(t)
	r := e.get("alice", "/ui/")
	if !regexp.MustCompile(`class="tile upstream" href="/ui/r/hub"`).MatchString(r.body) {
		t.Fatal("the proxy tile is not marked as upstream content")
	}
	if !strings.Contains(r.body, "caches https://registry-1.docker.io") {
		t.Fatal("the proxy tile does not name its upstream")
	}
	page := e.get("alice", "/ui/r/hub")
	if !strings.Contains(page.body, "callout upstream") || !strings.Contains(page.body, "never be promoted") {
		t.Fatal("the proxy page does not say its content is external")
	}
}

func TestUI_VirtualRepositoryListsOnlyReadableMembers(t *testing.T) {
	e := newEnv(t)
	r := e.get("alice", "/ui/r/all")
	if r.status != http.StatusOK {
		t.Fatalf("GET virtual: %d", r.status)
	}
	if !strings.Contains(r.body, `href="/ui/r/team-a"`) || !strings.Contains(r.body, `href="/ui/r/hub"`) {
		t.Fatal("the virtual page lacks members alice may read")
	}
	if strings.Contains(r.body, "team-b") {
		t.Fatal("the virtual page names team-b to alice")
	}
}

func TestUI_PyPIPackagePageGroupsFilesByVersion(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1.0", "2.0"} {
		if _, err := e.reg.Publish(t.Context(), registry.PublishParams{
			Repo: "py", Package: "demo", Version: v, Filename: "demo-" + v + ".tar.gz",
			Body: strings.NewReader("sdist " + v), Size: int64(len("sdist " + v)),
		}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	r := e.get("alice", "/ui/r/py/-/package?name=demo")
	if r.status != http.StatusOK || !strings.Contains(r.body, "demo-1.0.tar.gz") || !strings.Contains(r.body, "demo-2.0.tar.gz") {
		t.Fatalf("PyPI package page: %d\n%s", r.status, r.body)
	}
}

func TestUI_LoadMoreReturnsOnlyRows(t *testing.T) {
	e := newEnv(t)
	for i := range pageSize + 5 {
		e.push("team-a", "img"+string(rune('a'+i/26))+string(rune('a'+i%26)), "1.0", "x")
	}
	first := e.get("alice", "/ui/r/team-a")
	more := regexp.MustCompile(`hx-get="([^"]+)"`).FindStringSubmatch(first.body)
	if more == nil {
		t.Fatal("a repository with more than one page of images has no load-more control")
	}
	next := e.get("alice", strings.ReplaceAll(more[1], "&amp;", "&"), "HX-Request", "true")
	if next.status != http.StatusOK {
		t.Fatalf("load more: %d", next.status)
	}
	if strings.Contains(next.body, "<html") || strings.Contains(next.body, "site-header") {
		t.Fatal("load more returned a whole page, not rows")
	}
	if got := strings.Count(next.body, `class="row"`); got != 5 {
		t.Fatalf("load more returned %d rows, want the 5 left", got)
	}
	if next.header.Get("Vary") == "" || !strings.Contains(next.header.Get("Vary"), "HX-Request") {
		t.Fatal("pages that differ by HX-Request must say so in Vary")
	}
}

func TestUI_EveryResponseCarriesCSPAndNoStore(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-a", "app", "1.0", "h")
	for _, path := range []string{
		"/ui/", "/ui/r/team-a", "/ui/r/team-a/-/package?name=app",
		"/ui/r/team-a/-/artifact?digest=" + d.String(), "/ui/r/nope", "/ui/elsewhere",
	} {
		r := e.get("alice", path)
		if got := r.header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("%s: CSP %q", path, got)
		}
		if got := r.header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", path, got)
		}
		for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
			if r.header.Get(h) == "" {
				t.Errorf("%s: missing %s", path, h)
			}
		}
	}
}

func TestUI_StaticAssetsAreVersionedAndImmutable(t *testing.T) {
	e := newEnv(t)
	for _, file := range []string{"sindri.css", "htmx.min.js", "fonts/Geist-Variable.woff2"} {
		r := e.get("alice", "/ui/static/"+web.Version+"/"+file)
		if r.status != http.StatusOK || r.body == "" {
			t.Fatalf("%s: %d", file, r.status)
		}
		if !strings.Contains(r.header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: Cache-Control %q", file, r.header.Get("Cache-Control"))
		}
		if r.header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no CSP", file)
		}
	}
	if r := e.get("alice", "/ui/static/0000000000/sindri.css"); r.status != http.StatusNotFound {
		t.Fatalf("an asset under an old version: %d, want 404", r.status)
	}
	if r := e.get("alice", "/ui/static/"+web.Version+"/../web.go"); r.status == http.StatusOK {
		t.Fatal("a path outside the static directory was served")
	}
}

// Every route the UI registers must appear in this table with the access it
// grants. A new page added without a row fails here, which is the point: an
// unauthorized page is a security bug, not an omission (ADR 18).
func TestUI_EveryRouteHasAnAuthorizationCase(t *testing.T) {
	e := newEnv(t)
	d := e.push("team-b", "app", "1.0", "walk")
	cases := map[string]struct {
		path      string
		forbidden bool // alice must get 404
	}{
		"/":                   {path: "/ui/"},
		"/r/*":                {path: "/ui/r/team-b/-/artifact?digest=" + d.String(), forbidden: true},
		"/static/{version}/*": {path: "/ui/static/" + web.Version + "/sindri.css"},
	}
	var routes []string
	if err := chi.Walk(e.h.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			routes = append(routes, route)
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("no routes walked")
	}
	for _, route := range routes {
		c, ok := cases[route]
		if !ok {
			t.Errorf("route %s has no authorization case; add one to this table", route)
			continue
		}
		r := e.get("alice", c.path)
		if c.forbidden && r.status != http.StatusNotFound {
			t.Errorf("%s as alice: %d, want 404", c.path, r.status)
		}
		if r := e.get("bob", c.path); r.status != http.StatusOK {
			t.Errorf("%s as bob: %d, want 200", c.path, r.status)
		}
	}
}

// HEAD reaches the same handlers as GET; net/http drops the body.
func TestUI_HeadIsServed(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.srv)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", "alice")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("HEAD /ui/: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /ui/: %d", res.StatusCode)
	}
	if b, _ := io.ReadAll(res.Body); len(b) != 0 {
		t.Fatal("HEAD returned a body")
	}
}
