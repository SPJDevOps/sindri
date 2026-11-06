//go:build integration

package resolve_test

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: builds npm's dist.shasum for a stub upstream
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

const npmProxy = "npmjs"

// npmStub is an upstream registry. Documents are keyed by request URI, so a test
// sees exactly what Sindri asked for, escapes included.
type npmStub struct {
	mu        sync.Mutex
	documents map[string][]byte
	tarballs  map[string][]byte
	redirects map[string]string
	truncate  map[string]bool
	status    int // when non-zero, every response
	// tarballStatus, when non-zero, answers every request that is not for a
	// document, so a packument can list a tarball whose download then fails.
	tarballStatus int
	requests      []string
	accepts       []string
	check         func(*http.Request) bool // when set, a false answer is a 401
}

func (s *npmStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.RequestURI)
	s.accepts = append(s.accepts, r.Header.Get("Accept"))
	if s.check != nil && !s.check(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	if to, ok := s.redirects[r.RequestURI]; ok {
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	if body, ok := s.documents[r.RequestURI]; ok {
		_, _ = w.Write(body)
		return
	}
	if s.tarballStatus != 0 {
		w.WriteHeader(s.tarballStatus)
		return
	}
	if body, ok := s.tarballs[r.RequestURI]; ok {
		// Always declared, as a real registry does: what Sindri sends must not
		// depend on upstream being vague about its length.
		declared := len(body)
		if s.truncate[r.RequestURI] {
			declared += 10
		}
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		_, _ = w.Write(body)
		return
	}
	http.NotFound(w, r)
}

func (s *npmStub) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *npmStub) setDocument(uri string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.documents[uri] = body
}

func (s *npmStub) setTarball(uri string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tarballs[uri] = body
}

// configure changes the stub under its lock, for what has no setter of its own.
func (s *npmStub) configure(change func(*npmStub)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

func (s *npmStub) firstAccept() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepts[0]
}

func (s *npmStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// countOf is how many requests asked for uri.
func (s *npmStub) countOf(uri string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, got := range s.requests {
		if got == uri {
			n++
		}
	}
	return n
}

func newNPMStub() *npmStub {
	return &npmStub{
		documents: map[string][]byte{}, tarballs: map[string][]byte{},
		redirects: map[string]string{}, truncate: map[string]bool{},
	}
}

// npmStubEnv is a test environment with one npm proxy in front of a stub.
type npmStubEnv struct {
	*testEnv
	stub *npmStub
}

type npmProxyConfig struct {
	ttl, negativeTTL int32
	credentialsRef   string
	options          []envOption
}

func newNPMProxy(t *testing.T, cfg npmProxyConfig) npmStubEnv {
	t.Helper()
	return newNPMProxyAt(t, "", cfg)
}

// newNPMProxyAt configures the upstream URL with a base path.
func newNPMProxyAt(t *testing.T, basePath string, cfg npmProxyConfig) npmStubEnv {
	t.Helper()
	stub := newNPMStub()
	env := newEnv(t, stub.ServeHTTP, cfg.options...)
	if _, err := env.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: npmProxy, Type: model.RepoTypeProxy, Protocol: model.ProtocolNPM,
		UpstreamURL: env.upstream.URL + basePath, CredentialsRef: cfg.credentialsRef,
		TTLSeconds: cfg.ttl, NegativeTTL: cfg.negativeTTL,
	}); err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	return npmStubEnv{testEnv: env, stub: stub}
}

// get asks the adapter for a path under the proxy and returns status and body.
func (e npmStubEnv) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/npm/"+npmProxy+"/"+path, nil, "")
	return rec.Code, rec.Body.Bytes()
}

func (e npmStubEnv) mustGet(t *testing.T, path string) []byte {
	t.Helper()
	status, body := e.get(t, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, status, body)
	}
	return body
}

func (e npmStubEnv) wantStatus(t *testing.T, path string, want int) {
	t.Helper()
	status, body := e.get(t, path)
	if status != want {
		t.Fatalf("GET %s = %d %s, want %d", path, status, body, want)
	}
}

func (e npmStubEnv) placedFiles(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM files`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e npmStubEnv) cacheRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	if err := e.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM remote_cache WHERE cache_key = $1`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sri512(body []byte) string {
	sum := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func shasum(body []byte) string {
	sum := sha1.Sum(body) //nolint:gosec // G401: npm's dist.shasum, for a stub upstream
	return hex.EncodeToString(sum[:])
}

// npmDoc builds a packument. Each version gets the dist it is given.
func npmDoc(t *testing.T, name string, versions map[string]map[string]any, tags map[string]string) []byte {
	t.Helper()
	entries := map[string]any{}
	for v, dist := range versions {
		entries[v] = map[string]any{"name": name, "version": v, "dist": dist}
	}
	body, err := json.Marshal(map[string]any{"name": name, "dist-tags": tags, "versions": entries})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// serveOneVersion publishes name@1.0.0 on the stub with a good sha512.
func (e npmStubEnv) serveOneVersion(t *testing.T, uri, name string) []byte {
	t.Helper()
	base := name[strings.LastIndex(name, "/")+1:]
	tgz := []byte("tarball of " + name + "@1.0.0")
	tarballURI := "/" + name + "/-/" + base + "-1.0.0.tgz"
	e.stub.setTarball(tarballURI, tgz)
	e.stub.setDocument(uri, npmDoc(t, name, map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + tarballURI, "integrity": sri512(tgz), "shasum": shasum(tgz)},
	}, map[string]string{"latest": "1.0.0"}))
	return tgz
}

// The whole rewrite: dist.tarball points at the repository the client asked for,
// and every other byte of meaning survives, signatures above all.
func TestProxyNPM_PackumentRewritesOnlyTheTarball(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := []byte("scoped tarball")
	upstreamTarball := e.upstream.URL + "/@acme/new/-/new-1.0.0.tgz"
	raw, err := json.Marshal(map[string]any{
		"name":      "@acme/new",
		"modified":  "2026-01-01T00:00:00.000Z",
		"dist-tags": map[string]string{"latest": "1.0.0", "next": "1.0.0"},
		"versions": map[string]any{"1.0.0": map[string]any{
			"name": "@acme/new", "version": "1.0.0",
			"engines":      map[string]string{"node": ">=18"},
			"dependencies": map[string]string{"left-pad": "^1.0.0"},
			"dist": map[string]any{
				"tarball":      upstreamTarball,
				"integrity":    sri512(tgz),
				"shasum":       shasum(tgz),
				"fileCount":    3,
				"unpackedSize": 1234,
				"signatures":   []map[string]string{{"keyid": "SHA256:key", "sig": "signature"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.stub.setDocument("/@acme%2fnew", raw)

	var got struct {
		Name     string            `json:"name"`
		Modified string            `json:"modified"`
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Engines      map[string]string `json:"engines"`
			Dependencies map[string]string `json:"dependencies"`
			Dist         struct {
				Tarball      string              `json:"tarball"`
				Integrity    string              `json:"integrity"`
				Shasum       string              `json:"shasum"`
				FileCount    int                 `json:"fileCount"`
				UnpackedSize int                 `json:"unpackedSize"`
				Signatures   []map[string]string `json:"signatures"`
			} `json:"dist"`
		} `json:"versions"`
	}
	body := e.mustGet(t, "@acme%2fnew")
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(e.upstream.URL)) {
		t.Fatalf("the upstream's address reached the client: %s", body)
	}
	v := got.Versions["1.0.0"]
	if want := npmPublicURL + "/npm/" + npmProxy + "/@acme/new/-/new-1.0.0.tgz"; v.Dist.Tarball != want {
		t.Fatalf("tarball = %q, want %q", v.Dist.Tarball, want)
	}
	if v.Dist.Integrity != sri512(tgz) || v.Dist.Shasum != shasum(tgz) || v.Dist.FileCount != 3 || v.Dist.UnpackedSize != 1234 {
		t.Fatalf("dist = %+v", v.Dist)
	}
	if len(v.Dist.Signatures) != 1 || v.Dist.Signatures[0]["sig"] != "signature" {
		t.Fatalf("signatures = %+v", v.Dist.Signatures)
	}
	if v.Engines["node"] != ">=18" || v.Dependencies["left-pad"] != "^1.0.0" {
		t.Fatalf("manifest fields lost: %+v", v)
	}
	if got.Modified != "2026-01-01T00:00:00.000Z" || got.DistTags["next"] != "1.0.0" || got.Name != "@acme/new" {
		t.Fatalf("top-level fields lost: %+v", got)
	}
}

func TestProxyNPM_FetchesTheFullDocument(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.serveOneVersion(t, "/foo", "foo")
	e.mustGet(t, "foo")
	// The abbreviated document has no time map, and yarn 4 needs one.
	if got := e.stub.firstAccept(); got != "application/json" {
		t.Fatalf("Accept = %q, want the full document", got)
	}
}

func TestProxyNPM_ServesUpstreamTimes(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := []byte("tarball")
	tarballURI := "/foo/-/foo-1.0.0.tgz"
	e.stub.setTarball(tarballURI, tgz)
	dist := map[string]any{"tarball": e.upstream.URL + tarballURI, "integrity": sri512(tgz), "shasum": shasum(tgz)}

	read := func(t *testing.T, extra map[string]any) map[string]json.RawMessage {
		t.Helper()
		doc := map[string]any{
			"name": "foo", "dist-tags": map[string]string{"latest": "1.0.0"},
			"versions": map[string]any{"1.0.0": map[string]any{"name": "foo", "version": "1.0.0", "dist": dist}},
		}
		for k, v := range extra {
			doc[k] = v
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		e.stub.setDocument("/foo", raw)
		var got map[string]json.RawMessage
		if err := json.Unmarshal(e.mustGet(t, "foo"), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	times := map[string]string{"created": "2020-01-01T00:00:00.000Z", "modified": "2020-02-01T00:00:00.000Z", "1.0.0": "2020-02-01T00:00:00.000Z"}
	got := read(t, map[string]any{"time": times})
	var gotTimes map[string]string
	if err := json.Unmarshal(got["time"], &gotTimes); err != nil {
		t.Fatalf("time: %v", err)
	}
	for k, want := range times {
		if gotTimes[k] != want {
			t.Fatalf("time[%s] = %q, want %q", k, gotTimes[k], want)
		}
	}
}

func TestProxyNPM_UpstreamWithoutTimeStaysWithoutTime(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.serveOneVersion(t, "/foo", "foo")
	var got map[string]json.RawMessage
	if err := json.Unmarshal(e.mustGet(t, "foo"), &got); err != nil {
		t.Fatal(err)
	}
	if raw, ok := got["time"]; ok {
		t.Fatalf("time = %s, want none: Sindri never invents a timestamp", raw)
	}
}

func TestProxyNPM_CacheHitMakesNoUpstreamRequest(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := e.serveOneVersion(t, "/foo", "foo")

	e.mustGet(t, "foo")
	e.mustGet(t, "foo")
	if got := e.stub.count(); got != 1 {
		t.Fatalf("%d upstream requests after a cached packument, want 1", got)
	}
	first := e.mustGet(t, "foo/-/foo-1.0.0.tgz")
	if !bytes.Equal(first, tgz) {
		t.Fatalf("first download = %q", first)
	}
	before := e.stub.count()
	second := e.mustGet(t, "foo/-/foo-1.0.0.tgz")
	if !bytes.Equal(second, tgz) {
		t.Fatalf("cached download = %q", second)
	}
	if e.stub.count() != before {
		t.Fatal("a placed tarball contacted upstream")
	}
	if e.placedFiles(t) != 1 {
		t.Fatalf("%d files placed, want 1", e.placedFiles(t))
	}
}

func TestProxyNPM_PackumentRefreshesAfterTTL(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300})
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-1.0.0.tgz", "shasum": strings.Repeat("a", 40)},
	}, map[string]string{"latest": "1.0.0"}))
	e.mustGet(t, "foo")

	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-1.0.0.tgz", "shasum": strings.Repeat("a", 40)},
		"2.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-2.0.0.tgz", "shasum": strings.Repeat("b", 40)},
	}, map[string]string{"latest": "2.0.0"}))
	if body := e.mustGet(t, "foo"); bytes.Contains(body, []byte("2.0.0")) {
		t.Fatal("a fresh packument was refetched inside its TTL")
	}
	waitPastTTL()
	if body := e.mustGet(t, "foo"); !bytes.Contains(body, []byte("2.0.0")) {
		t.Fatalf("packument not refreshed after its TTL: %s", body)
	}
}

func TestProxyNPM_NotFoundIsCachedAndAnOutageIsNot(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})

	e.wantStatus(t, "absent", http.StatusNotFound)
	e.wantStatus(t, "absent", http.StatusNotFound)
	if got := e.stub.countOf("/absent"); got != 1 {
		t.Fatalf("%d upstream requests for a cached 404, want 1", got)
	}

	e.stub.setStatus(http.StatusServiceUnavailable)
	e.wantStatus(t, "flaky", http.StatusBadGateway)
	if n := e.cacheRows(t, registry.CacheKeyNPMPackument("flaky")); n != 0 {
		t.Fatalf("an outage wrote %d cache rows", n)
	}
	e.stub.setStatus(0)
	e.serveOneVersion(t, "/flaky", "flaky")
	e.mustGet(t, "flaky")
}

func TestProxyNPM_RefusalIsABadGatewayAndNotCached(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.stub.setStatus(http.StatusForbidden)
	e.wantStatus(t, "private", http.StatusBadGateway)
	if n := e.cacheRows(t, registry.CacheKeyNPMPackument("private")); n != 0 {
		t.Fatalf("a refusal wrote %d cache rows", n)
	}
}

func TestProxyNPM_StaleIsServedWhenUpstreamIsDown(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300})
	tgz := []byte("a tarball")
	other := []byte("another tarball")
	e.stub.setTarball("/foo/-/foo-1.0.0.tgz", tgz)
	e.stub.setTarball("/foo/-/foo-2.0.0.tgz", other)
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-1.0.0.tgz", "integrity": sri512(tgz)},
		"2.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-2.0.0.tgz", "integrity": sri512(other)},
	}, map[string]string{"latest": "2.0.0"}))
	e.mustGet(t, "foo")
	e.mustGet(t, "foo/-/foo-1.0.0.tgz")

	waitPastTTL()
	e.stub.setStatus(http.StatusServiceUnavailable)
	if body := e.mustGet(t, "foo"); !bytes.Contains(body, []byte("foo-2.0.0.tgz")) {
		t.Fatalf("stale packument = %s", body)
	}
	if body := e.mustGet(t, "foo/-/foo-1.0.0.tgz"); !bytes.Equal(body, tgz) {
		t.Fatalf("cached tarball = %q", body)
	}
	e.wantStatus(t, "foo/-/foo-2.0.0.tgz", http.StatusBadGateway)
}

// An upstream that answers 404 after a hit means the package is gone, which an
// outage does not. It is a miss, not a stale hit.
func TestProxyNPM_NotFoundAfterAHitIsNotServedStale(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300})
	e.serveOneVersion(t, "/foo", "foo")
	e.mustGet(t, "foo")
	waitPastTTL()
	e.stub.setStatus(http.StatusNotFound)
	e.wantStatus(t, "foo", http.StatusNotFound)
}

func TestProxyNPM_BadBytesAbortAndAreNotCached(t *testing.T) {
	good := []byte("the bytes the packument promised")
	cases := []struct {
		name     string
		dist     func(url string) map[string]any
		truncate bool
	}{
		{name: "WrongSHA512", dist: func(url string) map[string]any {
			return map[string]any{"tarball": url, "integrity": sri512(good), "shasum": shasum([]byte("served"))}
		}},
		{name: "WrongShasumAlone", dist: func(url string) map[string]any {
			return map[string]any{"tarball": url, "shasum": shasum(good)}
		}},
		{name: "Truncated", truncate: true, dist: func(url string) map[string]any {
			return map[string]any{"tarball": url, "integrity": sri512([]byte("served"))}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
			uri := "/foo/-/foo-1.0.0.tgz"
			e.stub.setTarball(uri, []byte("served"))
			e.stub.configure(func(s *npmStub) { s.truncate[uri] = tc.truncate })
			e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{"1.0.0": tc.dist(e.upstream.URL + uri)},
				map[string]string{"latest": "1.0.0"}))
			srv := e.serve(t)

			for attempt := 1; attempt <= 2; attempt++ {
				if _, err := fetch(t, srv, "/npm/"+npmProxy+"/foo/-/foo-1.0.0.tgz"); err == nil {
					t.Fatal("bad bytes must not complete as a clean response")
				}
				if got := e.stub.countOf(uri); got != attempt {
					t.Fatalf("attempt %d made %d upstream requests: bad bytes were cached", attempt, got)
				}
			}
			if n := e.placedFiles(t); n != 0 {
				t.Fatalf("%d files placed from bad bytes", n)
			}
		})
	}
}

// The hash is only known once the last byte has been read, and by then io.Copy has
// written it. If the response declared its length, that would be a completely
// framed response before the abort, and the client could not see the cut. An
// uncached stream is therefore chunked, so the abort comes before the terminating
// chunk. The tarball is large because net/http writes a declared-length body
// straight to the socket, where a small one might still be sitting in a buffer
// when the connection is cut, and pass this test for the wrong reason.
func TestProxyNPM_AbortOfALargeStreamIsVisibleToTheClient(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	uri := "/foo/-/foo-1.0.0.tgz"
	served := bytes.Repeat([]byte("not the promised bytes "), 20_000) // about 460 KB
	e.stub.setTarball(uri, served)
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + uri, "integrity": sri512([]byte("the promised bytes"))},
	}, map[string]string{"latest": "1.0.0"}))
	srv := e.serve(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/npm/"+npmProxy+"/foo/-/foo-1.0.0.tgz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v, want the headers to arrive so the stream starts", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength != -1 {
		t.Fatalf("an unverified stream declared %d bytes", resp.ContentLength)
	}
	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading %d of %d bytes ended with %v, want a transport error", len(got), len(served), err)
	}
	if n := e.placedFiles(t); n != 0 {
		t.Fatalf("%d files placed from bad bytes", n)
	}
}

// A placed tarball has been verified, so it can say how long it is, and a clean
// stream of a good one still arrives whole without saying. The tarball is larger
// than net/http's buffer: a body that fits and verifies gets a computed length
// from the server, which is safe, since by then the handler has finished cleanly.
func TestProxyNPM_OnlyAPlacedTarballDeclaresItsLength(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := bytes.Repeat([]byte("a verified tarball "), 20_000)
	uri := "/foo/-/foo-1.0.0.tgz"
	e.stub.setTarball(uri, tgz)
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + uri, "integrity": sri512(tgz)},
	}, map[string]string{"latest": "1.0.0"}))
	srv := e.serve(t)
	get := func() (int64, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/npm/"+npmProxy+"/foo/-/foo-1.0.0.tgz", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		return resp.ContentLength, body
	}

	if length, body := get(); length != -1 || !bytes.Equal(body, tgz) {
		t.Fatalf("first download: length %d, %d bytes", length, len(body))
	}
	// The placement commits as the last read returns, so give it that moment.
	for deadline := time.Now().Add(5 * time.Second); e.placedFiles(t) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the tarball was never placed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if length, body := get(); length != int64(len(tgz)) || !bytes.Equal(body, tgz) {
		t.Fatalf("cached download: length %d, %d bytes", length, len(body))
	}
}

// On a shared host the upstream's path is the tenant, so a packument cannot send
// the proxy's credentials to another path there. The request never goes out.
func TestProxyNPM_TarballOutsideTheUpstreamPathIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{name: "UnderTheBase", path: "/npm-a/foo/-/foo-1.0.0.tgz", wantOK: true},
		{name: "AnotherTenant", path: "/npm-b/foo/-/foo-1.0.0.tgz"},
		{name: "PrefixOfASegment", path: "/npm-ab/foo/-/foo-1.0.0.tgz"},
		{name: "DotDot", path: "/npm-a/../npm-b/foo-1.0.0.tgz"},
		{name: "EncodedDotDot", path: "/npm-a/%2e%2e/npm-b/foo-1.0.0.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newNPMProxyAt(t, "/npm-a", npmProxyConfig{
				ttl: 3600, negativeTTL: 300, credentialsRef: "mirror",
				options: []envOption{withCredentials(remote.StaticCredentials{"mirror": {Username: "mirror", Password: "s3cret"}})},
			})
			tgz := []byte("a tarball")
			e.stub.setDocument("/npm-a/foo", npmDoc(t, "foo", map[string]map[string]any{
				"1.0.0": {"tarball": e.upstream.URL + tc.path, "integrity": sri512(tgz)},
			}, map[string]string{"latest": "1.0.0"}))
			e.stub.setTarball("/npm-a/foo/-/foo-1.0.0.tgz", tgz)
			e.stub.setTarball(tc.path, tgz)

			if tc.wantOK {
				if body := e.mustGet(t, "foo/-/foo-1.0.0.tgz"); !bytes.Equal(body, tgz) {
					t.Fatalf("download = %q", body)
				}
				return
			}
			e.wantStatus(t, "foo/-/foo-1.0.0.tgz", http.StatusBadGateway)
			if got := e.stub.count(); got != 1 {
				t.Fatalf("%d upstream requests, want only the packument", got)
			}
		})
	}
}

func TestProxyNPM_SHA1OnlyLegacyPackageInstalls(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := []byte("a package from 2014")
	uri := "/old/-/old-0.1.0.tgz"
	e.stub.setTarball(uri, tgz)
	e.stub.setDocument("/old", npmDoc(t, "old", map[string]map[string]any{
		"0.1.0": {"tarball": e.upstream.URL + uri, "shasum": shasum(tgz)},
	}, map[string]string{"latest": "0.1.0"}))

	if body := e.mustGet(t, "old/-/old-0.1.0.tgz"); !bytes.Equal(body, tgz) {
		t.Fatalf("download = %q", body)
	}
	if n := e.placedFiles(t); n != 1 {
		t.Fatalf("%d files placed, want 1", n)
	}
}

func TestProxyNPM_VersionWithNoHashIsListedAndRefused(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	uri := "/foo/-/foo-1.0.0.tgz"
	e.stub.setTarball(uri, []byte("unverifiable"))
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + uri},
	}, map[string]string{"latest": "1.0.0"}))

	if body := e.mustGet(t, "foo"); !bytes.Contains(body, []byte(npmPublicURL+"/npm/"+npmProxy+"/foo/-/foo-1.0.0.tgz")) {
		t.Fatalf("a version with no hash must stay listed: %s", body)
	}
	e.wantStatus(t, "foo/-/foo-1.0.0.tgz", http.StatusBadGateway)
	if e.stub.countOf(uri) != 0 {
		t.Fatal("a tarball that cannot be verified was fetched")
	}
}

func TestProxyNPM_CrossHostTarballIsRefused(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(other.Close)

	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": other.URL + "/foo-1.0.0.tgz", "integrity": sri512([]byte("x"))},
	}, map[string]string{"latest": "1.0.0"}))

	if body := e.mustGet(t, "foo"); bytes.Contains(body, []byte(other.URL)) {
		t.Fatalf("the other host reached the client: %s", body)
	}
	e.wantStatus(t, "foo/-/foo-1.0.0.tgz", http.StatusBadGateway)
	if hits.Load() != 0 {
		t.Fatal("Sindri fetched from a host the packument named")
	}
}

// Credentials reach the upstream's origin for both the document and the tarball,
// and a redirect cannot carry a request, or them, anywhere else.
func TestProxyNPM_CredentialsStayOnUpstreamOrigin(t *testing.T) {
	var hits atomic.Int32
	var sawAuth atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
	}))
	t.Cleanup(other.Close)

	e := newNPMProxy(t, npmProxyConfig{
		ttl: 3600, negativeTTL: 300, credentialsRef: "mirror",
		options: []envOption{withCredentials(remote.StaticCredentials{"mirror": {Username: "mirror", Password: "s3cret"}})},
	})
	e.stub.configure(func(s *npmStub) {
		s.check = func(r *http.Request) bool {
			u, p, ok := r.BasicAuth()
			return ok && u == "mirror" && p == "s3cret"
		}
		s.redirects["/foo/-/foo-2.0.0.tgz"] = other.URL + "/foo-2.0.0.tgz"
		s.redirects["/foo"] = "/foo-document"
	})
	tgz := []byte("private tarball")
	e.stub.setTarball("/foo/-/foo-1.0.0.tgz", tgz)
	e.stub.setDocument("/foo-document", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-1.0.0.tgz", "integrity": sri512(tgz)},
		"2.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-2.0.0.tgz", "integrity": sri512([]byte("x"))},
	}, map[string]string{"latest": "2.0.0"}))

	e.mustGet(t, "foo")
	if body := e.mustGet(t, "foo/-/foo-1.0.0.tgz"); !bytes.Equal(body, tgz) {
		t.Fatalf("download = %q", body)
	}
	e.wantStatus(t, "foo/-/foo-2.0.0.tgz", http.StatusBadGateway)
	if hits.Load() != 0 || sawAuth.Load() {
		t.Fatalf("a redirect left the origin: %d hits, credentials seen: %v", hits.Load(), sawAuth.Load())
	}
}

func TestProxyNPM_BadDocumentIsUpstreamFailureAndNotCached(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{name: "NotJSON", body: []byte(`<html>maintenance</html>`)},
		{name: "AnotherPackage", body: []byte(`{"name":"bar","versions":{}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300})
			e.stub.setDocument("/foo", tc.body)
			e.wantStatus(t, "foo", http.StatusBadGateway)
			if n := e.cacheRows(t, registry.CacheKeyNPMPackument("foo")); n != 0 {
				t.Fatalf("a bad document wrote %d cache rows", n)
			}

			// Nor does it replace a good one: the last good document is served.
			e.serveOneVersion(t, "/foo", "foo")
			e.mustGet(t, "foo")
			waitPastTTL()
			e.stub.setDocument("/foo", tc.body)
			if body := e.mustGet(t, "foo"); !bytes.Contains(body, []byte("foo-1.0.0.tgz")) {
				t.Fatalf("stale document = %s", body)
			}
		})
	}
}

// logBuffer is a concurrency-safe log sink for asserting what was logged.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// paddedPackument is a valid packument for foo, padded with one unused field to
// at least size bytes.
func paddedPackument(t *testing.T, e npmStubEnv, size int) []byte {
	t.Helper()
	e.serveOneVersion(t, "/foo", "foo")
	var doc map[string]any
	if err := json.Unmarshal(e.stub.documents["/foo"], &doc); err != nil {
		t.Fatal(err)
	}
	doc["pad"] = strings.Repeat("x", size)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A packument is allowed to be far larger than a PyPI page or an OCI manifest:
// real ones are tens of megabytes. One past the old 32 MiB bound is served; one
// past the npm bound is a 502 that neither poisons the cache nor displaces a good
// copy, and the reason is logged.
func TestProxyNPM_LargePackumentIsServedAndOneBeyondTheBoundIsRefused(t *testing.T) {
	logs := &logBuffer{}
	const bound = 40 << 20
	e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300, options: []envOption{
		func(o *remote.Options) {
			o.Logger = slog.New(slog.NewTextHandler(logs, nil))
			o.MaxNPMPackumentBytes = bound
		},
	}})

	e.stub.setDocument("/foo", paddedPackument(t, e, 33<<20))
	if body := e.mustGet(t, "foo"); len(body) < 33<<20 {
		t.Fatalf("served %d bytes, want the whole 33 MiB document", len(body))
	}

	waitPastTTL()
	e.stub.setDocument("/foo", paddedPackument(t, e, bound))
	if body := e.mustGet(t, "foo"); len(body) > 33<<20+1<<20 {
		t.Fatalf("an oversize refresh displaced the good copy: %d bytes", len(body))
	}
	got := logs.String()
	for _, want := range []string{"repository=" + npmProxy, "package=foo", "reason=oversize"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Authorization") || strings.Contains(got, "xxxx") {
		t.Fatalf("log carries a header or body:\n%.500s", got)
	}

	// With no good copy there is nothing to fall back on.
	fresh := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300, options: []envOption{
		func(o *remote.Options) { o.MaxNPMPackumentBytes = bound },
	}})
	fresh.stub.setDocument("/foo", paddedPackument(t, fresh, bound))
	fresh.wantStatus(t, "foo", http.StatusBadGateway)
	if n := fresh.cacheRows(t, registry.CacheKeyNPMPackument("foo")); n != 0 {
		t.Fatalf("an oversize document wrote %d cache rows", n)
	}
}

func TestProxyNPM_FailureReasonsAreLoggedWithoutDetail(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e npmStubEnv)
		want  string
	}{
		{name: "Status", setup: func(e npmStubEnv) { e.stub.setStatus(http.StatusServiceUnavailable) }, want: "reason=status status=503"},
		{name: "Unparsable", setup: func(e npmStubEnv) { e.stub.setDocument("/foo", []byte("<html>secret-body</html>")) }, want: "reason=unparsable"},
		{name: "RefusedRedirect", setup: func(e npmStubEnv) {
			e.stub.configure(func(s *npmStub) { s.redirects["/foo"] = "http://elsewhere.invalid/foo" })
		}, want: "reason=\"redirect refused\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logBuffer{}
			e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300, options: []envOption{
				func(o *remote.Options) { o.Logger = slog.New(slog.NewTextHandler(logs, nil)) },
			}})
			tc.setup(e)
			e.wantStatus(t, "foo", http.StatusBadGateway)
			got := logs.String()
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "package=foo") || !strings.Contains(got, "repository="+npmProxy) {
				t.Fatalf("log lacks %q:\n%s", tc.want, got)
			}
			if strings.Contains(got, "secret-body") || strings.Contains(got, "elsewhere.invalid") {
				t.Fatalf("log carries upstream content:\n%s", got)
			}
		})
	}
}

// Everything an upstream writes is untrusted. What cannot safely reach a URL or a
// row is not listed, and the dist-tag naming it goes too.
func TestProxyNPM_HostileNamesNeverReachAURLOrARow(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	hash := sri512([]byte("x"))
	u := e.upstream.URL
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0":     {"tarball": u + "/foo/-/foo-1.0.0.tgz", "integrity": hash},
		"2.0.0":     {"tarball": u + "/foo/-/..%2f..%2fevil.tgz", "integrity": hash},
		"3.0.0":     {"tarball": u + "/foo/-/evil.sh", "integrity": hash},
		"4.0.0":     {"tarball": u + "/foo/-/a%22b.tgz", "integrity": hash},
		"../5.0.0":  {"tarball": u + "/foo/-/foo-5.0.0.tgz", "integrity": hash},
		"6.0.0\x00": {"tarball": u + "/foo/-/foo-6.0.0.tgz", "integrity": hash},
	}, map[string]string{"latest": "2.0.0", "stable": "1.0.0"}))

	var doc struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	body := e.mustGet(t, "foo")
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Versions) != 1 || doc.Versions["1.0.0"] == nil {
		t.Fatalf("versions = %s, want only 1.0.0", body)
	}
	if len(doc.DistTags) != 1 || doc.DistTags["stable"] != "1.0.0" {
		t.Fatalf("dist-tags = %v, want the tag on an omitted version gone", doc.DistTags)
	}
	for _, path := range []string{"foo/-/evil.sh", "foo/-/..%2f..%2fevil.tgz", "foo/-/foo-5.0.0.tgz", "foo/-/a%22b.tgz"} {
		if status, got := e.get(t, path); status != http.StatusNotFound {
			t.Fatalf("GET %s = %d %s, want 404", path, status, got)
		}
	}
}

// A name check that runs after the upstream call is no check at all: PackageClaimed
// reads a non-ASCII name as unclaimed, so the refusal has to come first.
func TestProxyNPM_NamesAreRefusedBeforeAnyUpstreamCall(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	paths := map[string]string{
		"KelvinSignInScope":        "@%E2%84%AAit%2fnew",
		"KelvinSignInScopePlain":   "@%E2%84%AAit/new",
		"KelvinSignInName":         "%E2%84%AAit",
		"NonASCIILetter":           "caf%C3%A9",
		"InvalidUTF8":              "caf%C3",
		"LeadingDot":               ".hidden",
		"LeadingDotInScopedName":   "@acme%2f.hidden",
		"LeadingDotInScope":        "@.acme%2fnew",
		"DotDot":                   "%2e%2e",
		"DoubleEncodedSpelling":    "%2540acme%252fnew",
		"NUL":                      "foo%00",
		"Newline":                  "foo%0abar",
		"Space":                    "foo%20bar",
		"QueryInName":              "foo%3Fx=1",
		"Backslash":                "foo%5cbar",
		"TooLong":                  strings.Repeat("a", 215),
		"TarballOfANonASCIIName":   "%E2%84%AAit/-/kit-1.0.0.tgz",
		"TarballWithANonASCIIFile": "kit/-/%E2%84%AAit-1.0.0.tgz",
		"TarballWithNUL":           "kit/-/kit%00.tgz",
		"DistTagsOfANonASCIIName":  "-/package/%E2%84%AAit/dist-tags",
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			if status, got := e.get(t, path); status != http.StatusNotFound {
				t.Fatalf("GET %s = %d %s, want 404", path, status, got)
			}
			if n := e.stub.count(); n != 0 {
				t.Fatalf("GET %s made %d upstream requests", path, n)
			}
		})
	}
}

// Every way a client can spell one scoped name reaches the resolver as one name,
// so it has one cache entry, one upstream URL, and one claim lookup.
func TestProxyNPM_EncodedSpellingsAreOneName(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := e.serveOneVersion(t, "/@acme%2fnew", "@acme/new")
	for _, spelling := range []string{"@acme%2fnew", "@acme%2Fnew", "%40acme%2fnew", "%40acme%2Fnew", "@acme/new", "%40acme/new"} {
		e.mustGet(t, spelling)
		if body := e.mustGet(t, spelling+"/-/new-1.0.0.tgz"); !bytes.Equal(body, tgz) {
			t.Fatalf("%s: download = %q", spelling, body)
		}
		e.mustGet(t, "-/package/"+strings.ReplaceAll(spelling, "/", "%2f")+"/dist-tags")
	}
	if got := e.stub.countOf("/@acme%2fnew"); got != 1 {
		t.Fatalf("%d upstream requests, want the spellings to share one", got)
	}
	if n := e.cacheRows(t, registry.CacheKeyNPMPackument("@acme/new")); n != 1 {
		t.Fatalf("%d cache rows under the decoded name, want 1", n)
	}
	if n := e.placedFiles(t); n != 1 {
		t.Fatalf("%d files placed, want 1", n)
	}
}

// Only a proxy read gets the legacy allowance. A hosted read stays as strict as
// publish, since nothing there could have that name.
func TestProxyNPM_LegacyNameReadsOnAProxyAndNotOnAHostedRepository(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := e.serveOneVersion(t, "/JSONStream", "JSONStream")
	if body := e.mustGet(t, "JSONStream"); !bytes.Contains(body, []byte("/npm/"+npmProxy+"/JSONStream/-/JSONStream-1.0.0.tgz")) {
		t.Fatalf("packument = %s", body)
	}
	if body := e.mustGet(t, "JSONStream/-/JSONStream-1.0.0.tgz"); !bytes.Equal(body, tgz) {
		t.Fatalf("download = %q", body)
	}
	if n := e.stub.countOf("/jsonstream"); n != 0 {
		t.Fatal("the name was folded on its way upstream")
	}

	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: "hosted", Type: model.RepoTypeHosted, Protocol: model.ProtocolNPM,
	}); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, http.MethodGet, "/npm/hosted/JSONStream", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("hosted read = %d, want 404", rec.Code)
	}
}

func TestProxyNPM_DistTagsComeFromTheSameDocument(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-1.0.0.tgz", "shasum": strings.Repeat("a", 40)},
	}, map[string]string{"latest": "1.0.0", "next": "1.0.0"}))

	var tags map[string]string
	if err := json.Unmarshal(e.mustGet(t, "-/package/foo/dist-tags"), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags["next"] != "1.0.0" {
		t.Fatalf("dist-tags = %v", tags)
	}
	e.mustGet(t, "foo")
	if got := e.stub.countOf("/foo"); got != 1 {
		t.Fatalf("%d upstream requests, want one document for both reads", got)
	}
	e.wantStatus(t, "-/package/absent/dist-tags", http.StatusNotFound)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPut, "/npm/" + npmProxy + "/-/package/foo/dist-tags/beta"},
		{http.MethodDelete, "/npm/" + npmProxy + "/-/package/foo/dist-tags/beta"},
		{http.MethodPut, "/npm/" + npmProxy + "/foo"},
	} {
		if rec := e.do(t, tc.method, tc.path, []byte(`"1.0.0"`), "application/json"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
}

func TestProxyNPM_TarballTheUpstreamDoesNotHaveIs404(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/foo/-/foo-1.0.0.tgz", "shasum": strings.Repeat("a", 40)},
	}, map[string]string{"latest": "1.0.0"}))
	e.wantStatus(t, "foo/-/foo-1.0.0.tgz", http.StatusNotFound)
	e.wantStatus(t, "foo/-/foo-9.9.9.tgz", http.StatusNotFound)
	e.wantStatus(t, "absent/-/absent-1.0.0.tgz", http.StatusNotFound)
}

// The filename is looked up in dist.tarball, not parsed: a registry may call its
// tarballs anything.
func TestProxyNPM_TarballIsFoundByItsBasename(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	tgz := []byte("oddly named")
	e.stub.setTarball("/store/abc123.tgz", tgz)
	e.stub.setDocument("/foo", npmDoc(t, "foo", map[string]map[string]any{
		"1.0.0": {"tarball": e.upstream.URL + "/store/abc123.tgz", "integrity": sri512(tgz)},
	}, map[string]string{"latest": "1.0.0"}))
	if body := e.mustGet(t, "foo"); !bytes.Contains(body, []byte("/npm/"+npmProxy+"/foo/-/abc123.tgz")) {
		t.Fatalf("packument = %s", body)
	}
	if body := e.mustGet(t, "foo/-/abc123.tgz"); !bytes.Equal(body, tgz) {
		t.Fatalf("download = %q", body)
	}
}

func TestProxyNPM_HeadOnAnUncachedTarballCachesNothing(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.serveOneVersion(t, "/foo", "foo")
	rec := e.do(t, http.MethodHead, "/npm/"+npmProxy+"/foo/-/foo-1.0.0.tgz", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", rec.Code)
	}
	if n := e.placedFiles(t); n != 0 {
		t.Fatalf("HEAD placed %d files", n)
	}
}

// Deduplication makes the blob physically present for every repository. Placing a
// tarball in one proxy must not make it readable from another (I2).
func TestProxyNPM_CachedTarballIsReadThroughPlacement(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 3600, negativeTTL: 300})
	e.serveOneVersion(t, "/foo", "foo")
	e.mustGet(t, "foo/-/foo-1.0.0.tgz")

	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: "second", Type: model.RepoTypeProxy, Protocol: model.ProtocolNPM,
		UpstreamURL: e.upstream.URL, TTLSeconds: 3600, NegativeTTL: 300,
	}); err != nil {
		t.Fatal(err)
	}
	e.stub.setStatus(http.StatusServiceUnavailable)
	rec := e.do(t, http.MethodGet, "/npm/second/foo/-/foo-1.0.0.tgz", nil, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("GET through the second proxy = %d %s, want 502", rec.Code, rec.Body.String())
	}
}

func TestProxyNPM_RetentionEvictsThePackumentAndItsFiles(t *testing.T) {
	e := newNPMProxy(t, npmProxyConfig{ttl: 60, negativeTTL: 300})
	tgz := e.serveOneVersion(t, "/foo", "foo")
	e.mustGet(t, "foo/-/foo-1.0.0.tgz")
	if n := e.placedFiles(t); n != 1 {
		t.Fatalf("%d files placed, want 1", n)
	}
	key := registry.CacheKeyNPMPackument("foo")
	if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remote_cache SET fetched_at = now() - interval '2 hours' WHERE cache_key = $1`, key); err != nil {
		t.Fatal(err)
	}

	res, err := e.reg.EvictStaleCache(t.Context(), time.Hour)
	if err != nil {
		t.Fatalf("EvictStaleCache: %v", err)
	}
	if res.Hits != 1 || e.placedFiles(t) != 0 || e.cacheRows(t, key) != 0 {
		t.Fatalf("evict = %+v, %d files, %d cache rows", res, e.placedFiles(t), e.cacheRows(t, key))
	}

	before := e.stub.count()
	if body := e.mustGet(t, "foo/-/foo-1.0.0.tgz"); !bytes.Equal(body, tgz) {
		t.Fatalf("download after eviction = %q", body)
	}
	if e.stub.count() == before {
		t.Fatal("an evicted tarball was served without contacting upstream")
	}
}
