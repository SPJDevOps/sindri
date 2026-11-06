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
	}, withCredentials(remote.StaticCredentials{"mirror-cred": {Username: "mirror", Password: "s3cret"}}))
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

// goneUpstream is a package index or packument whose listing a test changes
// between reads: the versions it lists, and a status that replaces the answer.
type goneUpstream struct {
	mu       sync.Mutex
	versions []string
	status   int    // when non-zero, answers the document request
	junk     bool   // when set, the document is not parseable
	doc      string // when non-empty, the document request answers this JSON verbatim
	// fileStatus, when non-zero, answers every file request (PyPI only).
	fileStatus int
}

func (u *goneUpstream) answerFiles(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fileStatus = status
}

func (u *goneUpstream) list(versions ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.versions, u.status, u.junk, u.doc = versions, 0, false, ""
}

func (u *goneUpstream) answer(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = status
}

// dropConnections makes the document request end without any response, which
// the client sees as a failed connection: the upstream not answering at all.
func (u *goneUpstream) dropConnections() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = dropConnection
}

// dropConnection is a status value no server sends, meaning "cut the connection".
const dropConnection = -1

func (u *goneUpstream) garble() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.junk = 0, true
}

// serveDoc makes the document request answer 200 with body, whatever it says.
func (u *goneUpstream) serveDoc(body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.junk, u.doc = 0, false, body
}

// goneProtocol describes one protocol's way of listing and fetching "demo".
type goneProtocol struct {
	name     string
	protocol model.Protocol
	prefix   string // URL prefix of a repository
	// handler serves the upstream for the versions listed.
	handler func(u *goneUpstream, base func() string) http.HandlerFunc
	// filePath is the path, under a repository, of the file of a version.
	filePath func(version string) string
	// docPath is the path, under a repository, of the package document.
	docPath string
}

func goneBody(version string) []byte { return []byte("bytes of demo " + version) }

func goneProtocols() []goneProtocol {
	return []goneProtocol{
		{
			name: "npm", protocol: model.ProtocolNPM, prefix: "/npm/",
			docPath:  "demo",
			filePath: func(v string) string { return "demo/-/demo-" + v + ".tgz" },
			handler: func(u *goneUpstream, base func() string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					u.mu.Lock()
					defer u.mu.Unlock()
					if r.URL.Path == "/demo" {
						switch {
						case u.status == dropConnection:
							panic(http.ErrAbortHandler)
						case u.status != 0:
							w.WriteHeader(u.status)
						case u.junk:
							_, _ = w.Write([]byte("not json"))
						case u.doc != "":
							_, _ = w.Write([]byte(u.doc))
						default:
							versions := map[string]any{}
							for _, v := range u.versions {
								body := goneBody(v)
								versions[v] = map[string]any{"name": "demo", "version": v, "dist": map[string]any{
									"tarball": base() + "/demo/-/demo-" + v + ".tgz", "integrity": sri512(body)}}
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"name": "demo", "versions": versions})
						}
						return
					}
					for _, v := range u.versions {
						if r.URL.Path == "/demo/-/demo-"+v+".tgz" {
							_, _ = w.Write(goneBody(v))
							return
						}
					}
					http.NotFound(w, r)
				}
			},
		},
		{
			name: "pypi", protocol: model.ProtocolPyPI, prefix: "/pypi/",
			docPath:  "simple/demo/",
			filePath: func(v string) string { return "packages/demo/demo-" + v + "-py3-none-any.whl" },
			handler: func(u *goneUpstream, _ func() string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					u.mu.Lock()
					defer u.mu.Unlock()
					if r.URL.Path == "/simple/demo/" {
						switch {
						case u.status == dropConnection:
							panic(http.ErrAbortHandler)
						case u.status != 0:
							w.WriteHeader(u.status)
						case u.junk:
							_, _ = w.Write([]byte("not json"))
						case u.doc != "":
							_, _ = w.Write([]byte(u.doc))
						default:
							var files []map[string]any
							for _, v := range u.versions {
								name := "demo-" + v + "-py3-none-any.whl"
								files = append(files, map[string]any{"filename": name, "url": "/files/" + name,
									"hashes": map[string]string{"sha256": shaHex(goneBody(v))}, "size": len(goneBody(v))})
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"name": "demo", "files": files})
						}
						return
					}
					for _, v := range u.versions {
						if r.URL.Path == "/files/demo-"+v+"-py3-none-any.whl" {
							if u.fileStatus != 0 {
								w.WriteHeader(u.fileStatus)
								return
							}
							_, _ = w.Write(goneBody(v))
							return
						}
					}
					http.NotFound(w, r)
				}
			},
		},
	}
}

type goneEnv struct {
	*testEnv
	p  goneProtocol
	up *goneUpstream
}

// newGoneEnv serves demo 1.0.0 and 2.0.0 through proxy "up" and virtual "all",
// reads both files so they are placed, and returns with the cache still fresh.
func newGoneEnv(t *testing.T, p goneProtocol, opts ...envOption) *goneEnv {
	t.Helper()
	up := &goneUpstream{versions: []string{"1.0.0", "2.0.0"}}
	var env *testEnv
	env = newEnv(t, p.handler(up, func() string { return env.upstream.URL }), opts...)
	env.createProxyAt(t, "up", p.protocol, env.upstream.URL+upstreamBase(p), "")
	env.createVirtualOf(t, "all", p.protocol, "up")
	e := &goneEnv{testEnv: env, p: p, up: up}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if got := e.status(t, "up", p.filePath(v)); got != http.StatusOK {
			t.Fatalf("priming %s = %d", v, got)
		}
	}
	return e
}

func upstreamBase(p goneProtocol) string {
	if p.protocol == model.ProtocolPyPI {
		return "/simple"
	}
	return ""
}

func (e *goneEnv) status(t *testing.T, repo, path string) int {
	t.Helper()
	return e.do(t, http.MethodGet, e.p.prefix+repo+"/"+path, nil, "").Code
}

// age makes the cached document older than the repository's TTL.
func (e *goneEnv) age(t *testing.T) {
	t.Helper()
	if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remote_cache SET fetched_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
}

func (e *goneEnv) flagged(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM files WHERE upstream_gone_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *goneEnv) setTTL(t *testing.T) {
	t.Helper()
	if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remotes SET ttl_seconds = 60, negative_ttl_seconds = 60`); err != nil {
		t.Fatal(err)
	}
}

func TestProxy_FileGoneUpstreamIsNotFound(t *testing.T) {
	cases := []struct {
		name    string
		virtual bool
		gone    func(u *goneUpstream)
		// removed and kept are the versions that must answer 404 and 200.
		removed, kept []string
	}{
		{"package gone, direct", false, func(u *goneUpstream) { u.answer(http.StatusNotFound) }, []string{"1.0.0", "2.0.0"}, nil},
		{"package gone, through a virtual", true, func(u *goneUpstream) { u.answer(http.StatusNotFound) }, []string{"1.0.0", "2.0.0"}, nil},
		{"one version gone, direct", false, func(u *goneUpstream) { u.list("2.0.0") }, []string{"1.0.0"}, []string{"2.0.0"}},
		{"one version gone, through a virtual", true, func(u *goneUpstream) { u.list("2.0.0") }, []string{"1.0.0"}, []string{"2.0.0"}},
	}
	for _, p := range goneProtocols() {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				e := newGoneEnv(t, p)
				e.setTTL(t)
				e.age(t)
				tc.gone(e.up)
				repo := "up"
				if tc.virtual {
					repo = "all"
				}
				for _, v := range tc.removed {
					if got := e.status(t, repo, p.filePath(v)); got != http.StatusNotFound {
						t.Errorf("GET %s %s = %d, want 404", repo, v, got)
					}
				}
				for _, v := range tc.kept {
					if got := e.status(t, repo, p.filePath(v)); got != http.StatusOK {
						t.Errorf("GET %s %s = %d, want 200", repo, v, got)
					}
				}
			})
		}
	}
}

func TestProxy_FileRelistedUpstreamServesAgain(t *testing.T) {
	for _, p := range goneProtocols() {
		t.Run(p.name, func(t *testing.T) {
			e := newGoneEnv(t, p)
			e.setTTL(t)
			e.age(t)
			e.up.list("2.0.0")
			if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusNotFound {
				t.Fatalf("removed file = %d, want 404", got)
			}
			if e.flagged(t) != 1 {
				t.Fatalf("%d files flagged, want 1", e.flagged(t))
			}
			e.up.list("1.0.0", "2.0.0")
			e.age(t)
			for _, repo := range []string{"up", "all"} {
				if got := e.status(t, repo, p.filePath("1.0.0")); got != http.StatusOK {
					t.Fatalf("relisted file through %s = %d, want 200", repo, got)
				}
			}
			if e.flagged(t) != 0 {
				t.Fatalf("%d files still flagged after relisting", e.flagged(t))
			}
		})
	}
}

func TestProxy_OutageOrRefusalFlagsNothing(t *testing.T) {
	cases := []struct {
		name string
		do   func(u *goneUpstream)
		only string // when set, the one protocol the case applies to
	}{
		{"server error", func(u *goneUpstream) { u.answer(http.StatusServiceUnavailable) }, ""},
		{"refused credential", func(u *goneUpstream) { u.answer(http.StatusForbidden) }, ""},
		{"unauthenticated", func(u *goneUpstream) { u.answer(http.StatusUnauthorized) }, ""},
		{"unparsable document", func(u *goneUpstream) { u.garble() }, ""},
		// A 200 that is not this project's document says nothing about its files.
		{"empty object", func(u *goneUpstream) { u.serveDoc(`{}`) }, ""},
		{"error message object", func(u *goneUpstream) { u.serveDoc(`{"message":"slow down"}`) }, ""},
		{"another project's document", func(u *goneUpstream) { u.serveDoc(`{"name":"other","files":[]}`) }, ""},
		// npm accepts a packument that lists no versions: it is a real answer.
		{"name without files", func(u *goneUpstream) { u.serveDoc(`{"name":"demo"}`) }, "pypi"},
	}
	for _, p := range goneProtocols() {
		for _, tc := range cases {
			if tc.only != "" && tc.only != p.name {
				continue
			}
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				e := newGoneEnv(t, p)
				e.setTTL(t)
				e.age(t)
				tc.do(e.up)
				for _, v := range []string{"1.0.0", "2.0.0"} {
					if got := e.status(t, "up", p.filePath(v)); got != http.StatusOK {
						t.Errorf("GET %s = %d, want the placed copy (200)", v, got)
					}
				}
				if e.flagged(t) != 0 {
					t.Errorf("%d files flagged by a non-answer", e.flagged(t))
				}
			})
		}
	}
}

// A document that is refused leaves the cached one serving: the project's
// listing is still the last good answer.
func TestProxy_RefusedDocumentKeepsServingTheStaleOne(t *testing.T) {
	for _, p := range goneProtocols() {
		t.Run(p.name, func(t *testing.T) {
			e := newGoneEnv(t, p)
			e.setTTL(t)
			e.up.serveDoc(`{"message":"slow down"}`)
			e.age(t)
			rec := e.do(t, http.MethodGet, p.prefix+"up/"+p.docPath, nil, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET document = %d, want the stale one (200)", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "1.0.0") || !strings.Contains(rec.Body.String(), "2.0.0") {
				t.Fatalf("document = %s, want the stale listing of both versions", rec.Body.String())
			}
			if e.flagged(t) != 0 {
				t.Fatalf("%d files flagged", e.flagged(t))
			}
		})
	}
}

// A lockfile install never reads the package document, so the tarball read has
// to be the one that notices.
func TestProxy_LockfileOnlyReadNoticesARemoval(t *testing.T) {
	for _, p := range goneProtocols() {
		t.Run(p.name, func(t *testing.T) {
			e := newGoneEnv(t, p)
			e.setTTL(t)
			e.age(t)
			e.up.list("2.0.0")
			if got := e.status(t, "up", p.filePath("1.0.0")); got != http.StatusNotFound {
				t.Fatalf("tarball read past the TTL = %d, want 404", got)
			}
			if got := e.status(t, "up", p.filePath("2.0.0")); got != http.StatusOK {
				t.Fatalf("the still-listed file = %d, want 200", got)
			}
		})
	}
}

// pypiMember starts a second upstream for a virtual's later member, and counts
// the file requests that reach it.
func (e *goneEnv) secondMember(t *testing.T, name string, versions ...string) (*goneUpstream, *atomic.Int32) {
	t.Helper()
	up := &goneUpstream{versions: versions}
	var files atomic.Int32
	inner := e.p.handler(up, func() string { return "" })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/files/") {
			files.Add(1)
		}
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	e.createProxyAt(t, name, e.p.protocol, srv.URL+upstreamBase(e.p), "")
	return up, &files
}

// A member that has the project owns the answer for its files: when its live
// index no longer lists one, a later member listing the same filename must not
// supply it (SIN-54). A member that lacks the project still moves the walk on.
func TestProxyPyPI_VirtualWalkStopsAtTheMemberThatDroppedTheFile(t *testing.T) {
	pypi := goneProtocols()[1]
	// How member A fails to supply 1.0.0, which B lists under the same name.
	const (
		dropped    = "index no longer lists the file"
		project404 = "project answers 404"
		file404    = "index lists it and the file URL answers 404"
		file503    = "index lists it and the file URL answers 503"
		file403    = "index lists it and the file URL answers 403"
	)
	cases := []struct {
		name   string
		mode   string
		nested bool
		want   int
		bFiles int32
		direct int // what a direct read of A answers
	}{
		{"dropped", dropped, false, http.StatusNotFound, 0, http.StatusNotFound},
		{"dropped, nested", dropped, true, http.StatusNotFound, 0, http.StatusNotFound},
		{"project 404 moves on to b", project404, false, http.StatusOK, 1, http.StatusNotFound},
		{"project 404 moves on to b, nested", project404, true, http.StatusOK, 1, http.StatusNotFound},
		{"file 404 is a's answer", file404, false, http.StatusNotFound, 0, http.StatusNotFound},
		{"file 404 is a's answer, nested", file404, true, http.StatusNotFound, 0, http.StatusNotFound},
		{"file outage fails the walk", file503, false, http.StatusBadGateway, 0, http.StatusBadGateway},
		{"file outage fails the walk, nested", file503, true, http.StatusBadGateway, 0, http.StatusBadGateway},
		{"file refusal fails the walk", file403, false, http.StatusBadGateway, 0, http.StatusBadGateway},
		{"file refusal fails the walk, nested", file403, true, http.StatusBadGateway, 0, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newGoneEnv(t, pypi)
			e.setTTL(t)
			_, bFiles := e.secondMember(t, "b", "1.0.0")
			e.age(t)
			switch tc.mode {
			case dropped:
				e.up.list("2.0.0")
			case project404:
				e.up.answer(http.StatusNotFound)
			default:
				e.up.answerFiles(map[string]int{file404: 404, file503: 503, file403: 403}[tc.mode])
			}
			if tc.mode != dropped {
				// Nothing of A's may answer for the file from a placed copy.
				if _, err := e.db.Pool().Exec(t.Context(), `DELETE FROM files WHERE blob_digest IN (SELECT digest FROM blobs)`); err != nil {
					t.Fatal(err)
				}
			}
			target := "walk"
			if tc.nested {
				e.createVirtualOf(t, "inner", pypi.protocol, "up")
				e.createVirtualOf(t, target, pypi.protocol, "inner", "b")
			} else {
				e.createVirtualOf(t, target, pypi.protocol, "up", "b")
			}
			if got := e.status(t, target, pypi.filePath("1.0.0")); got != tc.want {
				t.Fatalf("GET through the virtual = %d, want %d", got, tc.want)
			}
			if got := bFiles.Load(); got != tc.bFiles {
				t.Fatalf("%d file requests reached b, want %d", got, tc.bFiles)
			}
			if got := e.status(t, "up", pypi.filePath("1.0.0")); got != tc.direct {
				t.Fatalf("direct read of a = %d, want %d", got, tc.direct)
			}
		})
	}
}

// TestProxyPyPI_RelativeFileLinkResolvesAgainstTheProjectPage: PEP 503 resolves
// a relative file link against the URL of the page it came from, which is the
// URL that answered after redirects. devpi writes "../../+f/..." links, which
// reach the wrong directory if they are resolved against the upstream's base.
func TestProxyPyPI_RelativeFileLinkResolvesAgainstTheProjectPage(t *testing.T) {
	wheel := []byte("relative-link-wheel")
	const file = "rel_pkg-1.0.0-py3-none-any.whl"
	cases := []struct {
		name     string
		pagePath string // where the index document lives
		redirect bool   // /simple/rel-pkg/ redirects to pagePath
		link     string // the file link the index carries
		filePath string // where the file really is
	}{
		{"nested page with a parent-relative link", "/a/b/simple/rel-pkg/", false, "../../+f/ab/" + file, "/a/b/+f/ab/" + file},
		{"root-relative link", "/a/b/simple/rel-pkg/", false, "/files/" + file, "/files/" + file},
		{"page reached through a redirect", "/a/b/index/v2/rel-pkg/", true, "../../+f/ab/" + file, "/a/b/index/+f/ab/" + file},
		{"absolute link is left unchanged", "/a/b/simple/rel-pkg/", false, "@UPSTREAM@/abs/" + file, "/abs/" + file},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/a/b/simple/rel-pkg/":
					if tc.redirect {
						http.Redirect(w, r, tc.pagePath, http.StatusFound)
						return
					}
					fallthrough
				case tc.pagePath:
					_ = json.NewEncoder(w).Encode(map[string]any{
						"name": "rel-pkg",
						"files": []map[string]any{{
							"filename": file,
							"url":      strings.ReplaceAll(tc.link, "@UPSTREAM@", base),
							"hashes":   map[string]string{"sha256": shaHex(wheel)},
							"size":     len(wheel),
						}},
					})
				case tc.filePath:
					_, _ = w.Write(wheel)
				default:
					http.NotFound(w, r)
				}
			})
			base = env.upstream.URL
			if _, err := env.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
				Name: "proxy/rel", Type: model.RepoTypeProxy, Protocol: model.ProtocolPyPI,
				UpstreamURL: env.upstream.URL + "/a/b/simple", TTLSeconds: 3600, NegativeTTL: 300,
			}); err != nil {
				t.Fatalf("create proxy: %v", err)
			}
			// The second pass reads the cached index, which must resolve alike.
			for _, pass := range []string{"fresh index", "cached index"} {
				rec := env.do(t, http.MethodGet, "/pypi/proxy/rel/packages/rel-pkg/"+file, nil, "")
				if rec.Code != http.StatusOK || rec.Body.String() != string(wheel) {
					t.Fatalf("%s: download = %d %q, want the wheel", pass, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
