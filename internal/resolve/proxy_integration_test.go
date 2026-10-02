//go:build integration

package resolve_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

// waitPastTTL outlasts the one-second TTLs these tests configure.
func waitPastTTL() { time.Sleep(1200 * time.Millisecond) }

// fetch GETs path from a live server. It fails on a non-200 status and on a
// body that does not arrive cleanly, which is how an aborted stream shows up.
func fetch(t *testing.T, srv *httptest.Server, path string) ([]byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// testImage is an OCI image manifest with one layer.
type testImage struct {
	config, layer             []byte
	configDigest, layerDigest string
	manifest                  []byte
	manifestDigest            string
}

func newTestImage(t *testing.T, layer []byte, annotations map[string]string) testImage {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	img := testImage{
		config:       config,
		layer:        layer,
		configDigest: "sha256:" + shaHex(config),
		layerDigest:  "sha256:" + shaHex(layer),
	}
	body, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.Digest(img.configDigest),
			Size:      int64(len(config)),
		},
		Layers: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    digest.Digest(img.layerDigest),
			Size:      int64(len(layer)),
		}},
		Annotations: annotations,
	})
	if err != nil {
		t.Fatal(err)
	}
	img.manifest = body
	img.manifestDigest = "sha256:" + shaHex(body)
	return img
}

// pushImage pushes img to a hosted repository path through the OCI API.
func (e *testEnv) pushImage(t *testing.T, path, tag string, img testImage) {
	t.Helper()
	for _, blob := range [][]byte{img.config, img.layer} {
		rec := e.do(t, http.MethodPost, "/v2/"+path+"/blobs/uploads/?digest=sha256:"+shaHex(blob), blob, "application/octet-stream")
		if rec.Code != http.StatusCreated {
			t.Fatalf("blob upload to %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, http.MethodPut, "/v2/"+path+"/manifests/"+tag, img.manifest, ocispec.MediaTypeImageManifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("manifest push to %s: %d %s", path, rec.Code, rec.Body.String())
	}
}

// ociStub serves one image for any name and tag, counting requests by method.
type ociStub struct {
	mu       sync.Mutex
	image    testImage
	fail     bool
	byMethod map[string]int
}

func (s *ociStub) set(img testImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.image = img
}

func (s *ociStub) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func (s *ociStub) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byMethod[method]
}

func (s *ociStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.byMethod == nil {
		s.byMethod = map[string]int{}
	}
	s.byMethod[r.Method]++
	img, fail := s.image, s.fail
	s.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var body []byte
	switch {
	case strings.Contains(r.URL.Path, "/manifests/"):
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		body = img.manifest
	case strings.HasSuffix(r.URL.Path, "/blobs/"+img.layerDigest):
		body = img.layer
	case strings.HasSuffix(r.URL.Path, "/blobs/"+img.configDigest):
		body = img.config
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func (e *testEnv) createOCIProxy(t *testing.T, name string, ttl int) {
	t.Helper()
	e.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": name, "type": "proxy", "protocol": "oci",
		"upstream_url": e.upstream.URL, "ttl_seconds": ttl, "negative_ttl_seconds": ttl,
	})
}

// TestProxyPyPI_IndexListsUpstreamFilesAfterOneIsCached: caching one file must
// not hide the rest of the upstream project, and a release that appears
// upstream shows up once the index TTL expires.
func TestProxyPyPI_IndexListsUpstreamFilesAfterOneIsCached(t *testing.T) {
	wheels := map[string][]byte{
		"grow-1.0.0-py3-none-any.whl": []byte("wheel-one"),
		"grow-2.0.0-py3-none-any.whl": []byte("wheel-two"),
		"grow-3.0.0-py3-none-any.whl": []byte("wheel-three"),
	}
	var released atomic.Int32
	released.Store(2)
	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/simple/grow"):
			var files []map[string]any
			for v := 1; v <= int(released.Load()); v++ {
				name := fmt.Sprintf("grow-%d.0.0-py3-none-any.whl", v)
				files = append(files, map[string]any{
					"filename": name, "url": "/files/" + name,
					"hashes": map[string]string{"sha256": shaHex(wheels[name])},
					"size":   len(wheels[name]),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "grow", "files": files})
		case strings.HasPrefix(r.URL.Path, "/files/"):
			_, _ = w.Write(wheels[strings.TrimPrefix(r.URL.Path, "/files/")])
		default:
			http.NotFound(w, r)
		}
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "proxy/grow", "type": "proxy", "protocol": "pypi",
		"upstream_url": env.upstream.URL + "/simple", "ttl_seconds": 1, "negative_ttl_seconds": 1,
	})

	for _, accept := range []string{"application/vnd.pypi.simple.v1+json", "text/html"} {
		_ = env.do(t, http.MethodGet, "/pypi/proxy/grow/simple/grow/", nil, accept)
	}
	if dl := env.do(t, http.MethodGet, "/pypi/proxy/grow/packages/grow/grow-1.0.0-py3-none-any.whl", nil, ""); dl.Code != http.StatusOK {
		t.Fatalf("download: %d %s", dl.Code, dl.Body.String())
	}

	for _, accept := range []string{"application/vnd.pypi.simple.v1+json", "text/html"} {
		idx := env.do(t, http.MethodGet, "/pypi/proxy/grow/simple/grow/", nil, accept)
		for _, want := range []string{"grow-1.0.0-py3-none-any.whl", "grow-2.0.0-py3-none-any.whl"} {
			if !strings.Contains(idx.Body.String(), want) {
				t.Fatalf("%s index lost %s after 1.0.0 was cached: %s", accept, want, idx.Body.String())
			}
		}
	}

	released.Store(3)
	waitPastTTL()
	idx := env.do(t, http.MethodGet, "/pypi/proxy/grow/simple/grow/", nil, "application/vnd.pypi.simple.v1+json")
	if !strings.Contains(idx.Body.String(), "grow-3.0.0-py3-none-any.whl") {
		t.Fatalf("new upstream release missing after TTL: %s", idx.Body.String())
	}
}

// TestProxyOCI_TagRefreshesAfterTTL: a tag is mutable upstream, so an expired
// tag asks upstream again; when upstream is down the stale tag still answers,
// and a digest reference never needs upstream once cached.
func TestProxyOCI_TagRefreshesAfterTTL(t *testing.T) {
	stub := &ociStub{}
	v1 := newTestImage(t, []byte("layer"), map[string]string{"v": "1"})
	v2 := newTestImage(t, []byte("layer"), map[string]string{"v": "2"})
	stub.set(v1)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 1)

	get := func(ref string) *httptest.ResponseRecorder {
		t.Helper()
		return env.do(t, http.MethodGet, "/v2/hub/library/x/manifests/"+ref, nil, "")
	}
	if got := get("latest").Header().Get("Docker-Content-Digest"); got != v1.manifestDigest {
		t.Fatalf("first pull: %s, want %s", got, v1.manifestDigest)
	}

	stub.set(v2)
	if got := get("latest").Header().Get("Docker-Content-Digest"); got != v1.manifestDigest {
		t.Fatalf("within TTL the cached tag must answer: %s", got)
	}
	waitPastTTL()
	if got := get("latest").Header().Get("Docker-Content-Digest"); got != v2.manifestDigest {
		t.Fatalf("tag did not refresh after TTL: %s, want %s", got, v2.manifestDigest)
	}

	stub.setFail(true)
	waitPastTTL()
	before := stub.count(http.MethodGet)
	rec := get("latest")
	if rec.Code != http.StatusOK || rec.Header().Get("Docker-Content-Digest") != v2.manifestDigest {
		t.Fatalf("stale tag with upstream down: %d %s", rec.Code, rec.Header().Get("Docker-Content-Digest"))
	}
	if stub.count(http.MethodGet) == before {
		t.Fatal("an expired tag must ask upstream before falling back")
	}

	before = stub.count(http.MethodGet)
	if rec := get(v1.manifestDigest); rec.Code != http.StatusOK {
		t.Fatalf("cached digest reference: %d", rec.Code)
	}
	if stub.count(http.MethodGet) != before {
		t.Fatal("a cached digest reference is immutable and must not contact upstream")
	}
}

// TestProxyOCI_BlobStreamsBeforeUpstreamFinishes is the streaming guarantee:
// the client receives the first bytes of an uncached layer while upstream is
// still sending, and the finished layer is cached for the next pull.
func TestProxyOCI_BlobStreamsBeforeUpstreamFinishes(t *testing.T) {
	layer := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	layerDigest := "sha256:" + shaHex(layer)
	half := len(layer) / 2
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/blobs/"+layerDigest) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(layer)))
		_, _ = w.Write(layer[:half])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		_, _ = w.Write(layer[half:])
	})
	env.createOCIProxy(t, "hub", 3600)
	srv := env.serve(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v2/hub/library/big/blobs/"+layerDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)

	// Less than the half upstream has sent, so bytes still sitting in a
	// server write buffer cannot make this flaky. The deadline covers the
	// response headers too: a buffering proxy withholds those, not the body.
	first := make([]byte, half-8<<10)
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			got <- result{err: fmt.Errorf("status %d", resp.StatusCode)}
			return
		}
		_, err = io.ReadFull(resp.Body, first)
		got <- result{resp: resp, err: err}
	}()
	var resp *http.Response
	select {
	case res := <-got:
		if res.err != nil {
			t.Fatalf("reading first bytes: %v", res.err)
		}
		resp = res.resp
	case <-time.After(5 * time.Second):
		t.Fatal("no bytes reached the client while upstream was mid-body: the proxy is buffering, not streaming")
	}
	defer func() { _ = resp.Body.Close() }()
	releaseOnce.Do(func() { close(release) })
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading rest: %v", err)
	}
	if !bytes.Equal(append(first, rest...), layer) {
		t.Fatal("streamed bytes differ from upstream")
	}

	before := env.requests.Load()
	body, err := fetch(t, srv, "/v2/hub/library/big/blobs/"+layerDigest)
	if err != nil || !bytes.Equal(body, layer) {
		t.Fatalf("cached pull: %v", err)
	}
	if env.requests.Load() != before {
		t.Fatal("second pull of a streamed layer contacted upstream: it was not cached")
	}
}

// TestProxyOCI_BadBlobAbortsAndIsNotCached: bytes that do not hash to the
// requested digest cut the client's connection and are never placed.
func TestProxyOCI_BadBlobAbortsAndIsNotCached(t *testing.T) {
	claimed := "sha256:" + shaHex([]byte("the real layer"))
	env := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("something else"))
	})
	env.createOCIProxy(t, "hub", 3600)
	srv := env.serve(t)

	if _, err := fetch(t, srv, "/v2/hub/library/x/blobs/"+claimed); err == nil {
		t.Fatal("a blob failing verification must not complete as a clean response")
	}
	d, err := model.ParseDigest(claimed)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.reg.CheckPlacement(t.Context(), "hub", d); !errors.Is(err, registry.ErrNotPlaced) {
		t.Fatalf("bad blob placed: %v", err)
	}
}

// TestProxyOCI_HeadAndRangeOnUncachedBlob: HEAD asks upstream with HEAD and
// caches nothing; a ranged GET caches the whole blob and serves the slice.
func TestProxyOCI_HeadAndRangeOnUncachedBlob(t *testing.T) {
	stub := &ociStub{}
	img := newTestImage(t, []byte("0123456789"), nil)
	stub.set(img)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)
	d, err := model.ParseDigest(img.layerDigest)
	if err != nil {
		t.Fatal(err)
	}

	head := env.do(t, http.MethodHead, "/v2/hub/library/x/blobs/"+img.layerDigest, nil, "")
	if head.Code != http.StatusOK || head.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD: %d length %q", head.Code, head.Header().Get("Content-Length"))
	}
	if stub.count(http.MethodGet) != 0 {
		t.Fatal("HEAD downloaded the blob from upstream")
	}
	if err := env.reg.CheckPlacement(t.Context(), "hub", d); !errors.Is(err, registry.ErrNotPlaced) {
		t.Fatalf("HEAD placed the blob: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/hub/library/x/blobs/"+img.layerDigest, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range %q", got)
	}
	if err := env.reg.CheckPlacement(t.Context(), "hub", d); err != nil {
		t.Fatalf("ranged GET should cache the blob: %v", err)
	}
}

// TestProxyOCI_BearerChallenge exercises the Docker Hub shape: a 401 with a
// bearer challenge, a token endpoint, one token reused across requests for
// the same image, and a name the upstream denies answered as unknown, not as
// a server error.
func TestProxyOCI_BearerChallenge(t *testing.T) {
	img := newTestImage(t, []byte("layer"), nil)
	var tokens atomic.Int32
	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "tok|" + r.URL.Query().Get("scope"), "expires_in": 300,
			})
			return
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		scope := "repository:" + name + ":pull"
		// Docker Hub denies unknown names with 401 even to a valid token.
		if r.Header.Get("Authorization") != "Bearer tok|"+scope || name != "library/alpine" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="http://`+r.Host+`/token",service="stub",scope="`+scope+`"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		_, _ = w.Write(img.manifest)
	})
	env.createOCIProxy(t, "hub", 3600)

	for _, tag := range []string{"latest", "3.20"} {
		rec := env.do(t, http.MethodGet, "/v2/hub/library/alpine/manifests/"+tag, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("pull %s: %d %s", tag, rec.Code, rec.Body.String())
		}
	}
	if n := tokens.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times for one image; the token was not reused", n)
	}

	denied := env.do(t, http.MethodGet, "/v2/hub/library/nosuch/manifests/latest", nil, "")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("denied name: %d %s, want 404", denied.Code, denied.Body.String())
	}
}

// TestProxyOCI_PlaceholderNeverOverwritesBlobSize: a proxied manifest records
// its children before their bytes arrive. That record must not rewrite the
// size of a blob another repository already holds, and must take its size
// from the descriptor rather than claim zero.
func TestProxyOCI_PlaceholderNeverOverwritesBlobSize(t *testing.T) {
	shared := []byte("a layer both repositories have")
	sharedDigest := "sha256:" + shaHex(shared)
	unfetched := "sha256:" + shaHex([]byte("never fetched"))
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(unfetched), Size: 777,
		},
		// The descriptor misstates the size; the stored bytes are the truth.
		Layers: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageLayer, Digest: digest.Digest(sharedDigest), Size: 999,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		_, _ = w.Write(manifest)
	})
	env.createOCIProxy(t, "hub", 3600)
	env.createJSON(t, "/api/v1/repositories", map[string]any{"name": "internal-oci", "type": "hosted", "protocol": "oci"})

	up := env.do(t, http.MethodPost, "/v2/internal-oci/blobs/uploads/?digest="+sharedDigest, shared, "application/octet-stream")
	if up.Code != http.StatusCreated {
		t.Fatalf("hosted blob upload: %d %s", up.Code, up.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/v2/hub/library/x/manifests/latest", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("proxy manifest: %d %s", rec.Code, rec.Body.String())
	}

	q := env.db.Queries()
	sharedRow, err := q.GetBlob(t.Context(), sharedDigest)
	if err != nil {
		t.Fatal(err)
	}
	if sharedRow.Size != int64(len(shared)) {
		t.Fatalf("existing blob size rewritten to %d, want %d", sharedRow.Size, len(shared))
	}
	placeholder, err := q.GetBlob(t.Context(), unfetched)
	if err != nil {
		t.Fatal(err)
	}
	if placeholder.Size != 777 {
		t.Fatalf("placeholder size %d, want the descriptor's 777", placeholder.Size)
	}
}

// TestProxyPyPI_CredentialsStayOnUpstreamOrigin: a proxy's credentials go to
// the upstream the operator configured, and never follow an index link to
// another host.
func TestProxyPyPI_CredentialsStayOnUpstreamOrigin(t *testing.T) {
	offsite := []byte("offsite-wheel")
	onsite := []byte("onsite-wheel")
	var offsiteSawAuth atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			offsiteSawAuth.Store(true)
		}
		_, _ = w.Write(offsite)
	}))
	t.Cleanup(other.Close)

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "mirror" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/simple/private-pkg"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "private-pkg",
				"files": []map[string]any{
					{
						"filename": "private_pkg-1.0.0-py3-none-any.whl",
						"url":      other.URL + "/private_pkg-1.0.0-py3-none-any.whl",
						"hashes":   map[string]string{"sha256": shaHex(offsite)},
						"size":     len(offsite),
					},
					{
						"filename": "private_pkg-2.0.0-py3-none-any.whl",
						"url":      "/files/private_pkg-2.0.0-py3-none-any.whl",
						"hashes":   map[string]string{"sha256": shaHex(onsite)},
						"size":     len(onsite),
					},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/files/"):
			_, _ = w.Write(onsite)
		default:
			http.NotFound(w, r)
		}
	}, withCredentials(remote.StaticCredentials{"mirror-cred": {"mirror", "s3cret"}}))
	// The management API refuses credentials_ref until M7 supplies secrets
	// (ADR 15), so the repository is created below the API. What this guards,
	// credentials never leaving the upstream's origin, is what M7 builds on.
	if _, err := env.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: "proxy/private", Type: model.RepoTypeProxy, Protocol: model.ProtocolPyPI,
		UpstreamURL: env.upstream.URL + "/simple", CredentialsRef: "mirror-cred",
		TTLSeconds: 3600, NegativeTTL: 300,
	}); err != nil {
		t.Fatalf("create proxy: %v", err)
	}

	for _, file := range []string{"private_pkg-1.0.0-py3-none-any.whl", "private_pkg-2.0.0-py3-none-any.whl"} {
		if rec := env.do(t, http.MethodGet, "/pypi/proxy/private/packages/private-pkg/"+file, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("download %s: %d %s", file, rec.Code, rec.Body.String())
		}
	}
	if offsiteSawAuth.Load() {
		t.Fatal("upstream credentials were sent to a host the index linked to")
	}
}

// TestAPI_RejectsUnknownCredentialsRef: a credentials_ref the server's
// credentials section does not define is refused when the repository is
// created, rather than accepted and broken on the first pull (ADR 15).
func TestAPI_RejectsUnknownCredentialsRef(t *testing.T) {
	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	body, err := json.Marshal(map[string]any{
		"name": "docker/fancy", "type": "proxy", "protocol": "oci",
		"upstream_url": env.upstream.URL, "credentials_ref": "fancysecret",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := env.do(t, http.MethodPost, "/api/v1/repositories", body, "application/json")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "fancysecret") ||
		!strings.Contains(rec.Body.String(), "credentials section") {
		t.Fatalf("create with unknown credentials_ref: %d %s, want 400 naming the reference", rec.Code, rec.Body.String())
	}
	if _, err := env.reg.GetRepository(t.Context(), "docker/fancy"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("rejected repository was created anyway: %v", err)
	}
}

// TestProxyOCI_UpstreamManifestDigestMismatchIsBadGateway: upstream answering
// a digest reference with other bytes is the upstream's failure, reported as
// 502 and never cached.
func TestProxyOCI_UpstreamManifestDigestMismatchIsBadGateway(t *testing.T) {
	stub := &ociStub{}
	stub.set(newTestImage(t, []byte("layer"), nil))
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)

	wrong := "sha256:" + shaHex([]byte("a different manifest"))
	rec := env.do(t, http.MethodGet, "/v2/hub/library/x/manifests/"+wrong, nil, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("digest mismatch: %d %s, want 502", rec.Code, rec.Body.String())
	}
}

// TestDependencyConfusion_OCIHostedImageIsNeverServedFromProxy is the OCI half
// of the permanent I5 regression guard: an image a hosted member claims is
// resolved only there, even with the proxy listed first, and a tag the hosted
// member lacks is unknown rather than fetched from upstream.
func TestDependencyConfusion_OCIHostedImageIsNeverServedFromProxy(t *testing.T) {
	hostile := newTestImage(t, []byte("hostile layer"), map[string]string{"from": "upstream"})
	stub := &ociStub{}
	stub.set(hostile)
	env := newEnv(t, stub.ServeHTTP)
	env.createOCIProxy(t, "hub", 3600)
	env.createJSON(t, "/api/v1/repositories", map[string]any{"name": "internal-oci", "type": "hosted", "protocol": "oci"})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "docker", "type": "virtual", "protocol": "oci",
		"members": []string{"hub", "internal-oci"},
	})
	safe := newTestImage(t, []byte("our layer"), map[string]string{"from": "hosted"})
	env.pushImage(t, "internal-oci/team/app", "1.0", safe)

	before := env.requests.Load()
	rec := env.do(t, http.MethodGet, "/v2/docker/team/app/manifests/1.0", nil, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Docker-Content-Digest") != safe.manifestDigest {
		t.Fatalf("claimed image: %d %s", rec.Code, rec.Header().Get("Docker-Content-Digest"))
	}
	if rec := env.do(t, http.MethodGet, "/v2/docker/team/app/manifests/99.0", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("tag only upstream offers: %d, want 404", rec.Code)
	}
	if env.requests.Load() != before {
		t.Fatalf("a hosted-claimed image contacted upstream: before=%d after=%d", before, env.requests.Load())
	}
}
