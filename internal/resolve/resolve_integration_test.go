//go:build integration

package resolve_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/proto/oci"
	"github.com/spj/sindri/internal/proto/pypi"
	"github.com/spj/sindri/internal/reconcile"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const testToken = "m5-e2e-token"

type testEnv struct {
	handler  http.Handler
	reg      *registry.Registry
	db       *db.DB
	upstream *httptest.Server
	requests *atomic.Int64
}

// envOption adjusts the remote client a test environment is built with.
type envOption func(*remote.Options)

func withCredentials(c remote.Credentials) envOption {
	return func(o *remote.Options) { o.Credentials = c }
}

func newEnv(t *testing.T, upstream http.HandlerFunc, options ...envOption) *testEnv {
	t.Helper()
	var requests atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(up.Close)

	url := testsupport.StartPostgres(t)
	opts := testsupport.DBOptions(url)
	if err := db.Migrate(t.Context(), opts, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	database, err := db.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(database.Close)

	blobs := memstore.New()
	casStore := cas.New(blobs)
	reg := registry.New(database, casStore, "memory")
	remoteOpts := remote.Options{
		Registry:   reg,
		HTTPClient: up.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range options {
		opt(&remoteOpts)
	}
	remoteClient := remote.New(remoteOpts)
	resolver := resolve.New(reg, remoteClient)
	authenticator := auth.NewStaticBearer(testToken)

	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		API: api.Handler(api.Options{
			Registry:  reg,
			Auth:      authenticator,
			RequestID: server.RequestIDFromContext,
		}),
		OCI: oci.Handler(oci.Options{
			Resolver: resolver,
			CAS:      casStore,
		}),
		PyPI:   pypi.Handler(pypi.Options{Resolver: resolver}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return &testEnv{handler: srv.Handler(), reg: reg, db: database, upstream: up, requests: &requests}
}

func (e *testEnv) do(t *testing.T, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, r)
	if contentType != "" {
		if method == http.MethodGet || method == http.MethodHead {
			req.Header.Set("Accept", contentType)
		} else {
			req.Header.Set("Content-Type", contentType)
		}
	}
	if strings.HasPrefix(path, "/api/") {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// serve exposes the handler over a real listener, for tests that need a
// response to stream or a connection to be cut; a ResponseRecorder does
// neither.
func (e *testEnv) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)
	return srv
}

func (e *testEnv) createJSON(t *testing.T, path string, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, http.MethodPost, path, body, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
	}
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestProxyPyPI_InstallAndCacheHit covers gate items 1 and 2: install through
// a proxy succeeds, and the second install makes zero upstream requests.
func TestProxyPyPI_InstallAndCacheHit(t *testing.T) {
	wheel := []byte("wheel-bytes-v1")
	filename := "demo_package-1.0.0-py3-none-any.whl"
	hexSum := shaHex(wheel)

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/demo-package/") || strings.HasSuffix(r.URL.Path, "/demo-package"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":     "demo-package",
				"versions": []string{"1.0.0"},
				"files": []map[string]any{{
					"filename": filename,
					"url":      "/files/" + filename,
					"hashes":   map[string]string{"sha256": hexSum},
					"size":     len(wheel),
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/"+filename):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(wheel)
		default:
			http.NotFound(w, r)
		}
	})

	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name":                 "proxy/pypi-public",
		"type":                 "proxy",
		"protocol":             "pypi",
		"upstream_url":         env.upstream.URL + "/simple",
		"ttl_seconds":          3600,
		"negative_ttl_seconds": 300,
	})

	idx := env.do(t, http.MethodGet, "/pypi/proxy/pypi-public/simple/demo-package/", nil,
		"application/vnd.pypi.simple.v1+json")
	if idx.Code != http.StatusOK {
		t.Fatalf("simple index: %d %s", idx.Code, idx.Body.String())
	}
	if !strings.Contains(idx.Body.String(), filename) {
		t.Fatalf("index missing file: %s", idx.Body.String())
	}

	dl := env.do(t, http.MethodGet, "/pypi/proxy/pypi-public/packages/demo-package/"+filename, nil, "")
	if dl.Code != http.StatusOK {
		t.Fatalf("download: %d %s", dl.Code, dl.Body.String())
	}
	if !bytes.Equal(dl.Body.Bytes(), wheel) {
		t.Fatalf("download bytes mismatch")
	}
	first := env.requests.Load()
	if first == 0 {
		t.Fatal("expected upstream requests on first install")
	}

	idx2 := env.do(t, http.MethodGet, "/pypi/proxy/pypi-public/simple/demo-package/", nil,
		"application/vnd.pypi.simple.v1+json")
	if idx2.Code != http.StatusOK {
		t.Fatalf("cached index: %d", idx2.Code)
	}
	dl2 := env.do(t, http.MethodGet, "/pypi/proxy/pypi-public/packages/demo-package/"+filename, nil, "")
	if dl2.Code != http.StatusOK {
		t.Fatalf("cached download: %d", dl2.Code)
	}
	if env.requests.Load() != first {
		t.Fatalf("second install contacted upstream: before=%d after=%d", first, env.requests.Load())
	}
}

// TestVirtual_HostedWinsOverProxy covers gate item 3.
func TestVirtual_HostedWinsOverProxy(t *testing.T) {
	hostedWheel := []byte("hosted-wheel")
	proxyWheel := []byte("proxy-wheel-should-not-win")
	filename := "shared_pkg-1.0.0-py3-none-any.whl"

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/shared-pkg"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "shared-pkg",
				"files": []map[string]any{{
					"filename": filename,
					"url":      "/files/" + filename,
					"hashes":   map[string]string{"sha256": shaHex(proxyWheel)},
					"size":     len(proxyWheel),
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/"+filename):
			_, _ = w.Write(proxyWheel)
		default:
			http.NotFound(w, r)
		}
	})

	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "hosted/internal", "type": "hosted", "protocol": "pypi",
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "proxy/pypi-public", "type": "proxy", "protocol": "pypi",
		"upstream_url": env.upstream.URL + "/simple",
		"ttl_seconds":  3600, "negative_ttl_seconds": 300,
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "python", "type": "virtual", "protocol": "pypi",
		"members": []string{"hosted/internal", "proxy/pypi-public"},
	})

	// Publish hosted package directly through the registry (management API
	// path params do not accept slash-containing repository names as a single
	// segment; create still takes the name in the JSON body).
	_, err := env.reg.Publish(t.Context(), registry.PublishParams{
		Repo: "hosted/internal", Package: "shared-pkg", Version: "1.0.0",
		Filename: filename, Body: bytes.NewReader(hostedWheel), Size: int64(len(hostedWheel)),
		Immutable: true,
	})
	if err != nil {
		t.Fatalf("publish hosted: %v", err)
	}

	before := env.requests.Load()
	dl := env.do(t, http.MethodGet, "/pypi/python/packages/shared-pkg/"+filename, nil, "")
	if dl.Code != http.StatusOK {
		t.Fatalf("virtual download: %d %s", dl.Code, dl.Body.String())
	}
	if !bytes.Equal(dl.Body.Bytes(), hostedWheel) {
		t.Fatalf("expected hosted bytes, got %q", dl.Body.String())
	}
	if env.requests.Load() != before {
		t.Fatalf("virtual resolution contacted upstream")
	}
}

// TestDependencyConfusion_HostedNameIsNeverServedFromProxy is the permanent
// I5 regression guard. Upstream offers 99.0.0 of a name the hosted member
// claims at 1.0.0; resolution returns hosted and never contacts upstream.
// ARCHITECTURE.md section 4 / invariant I5.
func TestDependencyConfusion_HostedNameIsNeverServedFromProxy(t *testing.T) {
	t.Run("HostedFirst", func(t *testing.T) {
		runDependencyConfusion(t, []string{"hosted/internal", "proxy/pypi-public"}, "hf")
	})
	t.Run("ProxyFirstStillClaims", func(t *testing.T) {
		runDependencyConfusion(t, []string{"proxy/pypi-public", "hosted/internal"}, "pf")
	})
}

func runDependencyConfusion(t *testing.T, members []string, tag string) {
	t.Helper()
	hostedWheel := []byte("safe-1.0.0")
	hostileWheel := []byte("hostile-99.0.0")
	hostedFile := "our_internal_lib-1.0.0-py3-none-any.whl"
	hostileFile := "our_internal_lib-99.0.0-py3-none-any.whl"

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/our-internal-lib"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "our-internal-lib",
				"files": []map[string]any{{
					"filename": hostileFile,
					"url":      "/files/" + hostileFile,
					"hashes":   map[string]string{"sha256": shaHex(hostileWheel)},
					"size":     len(hostileWheel),
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/"+hostileFile):
			_, _ = w.Write(hostileWheel)
		default:
			http.NotFound(w, r)
		}
	})

	hostedName := "hosted/internal-" + tag
	proxyName := "proxy/pypi-" + tag
	virtName := "python-" + tag
	mapped := make([]string, len(members))
	for i, m := range members {
		if strings.HasPrefix(m, "hosted/") {
			mapped[i] = hostedName
		} else {
			mapped[i] = proxyName
		}
	}

	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": hostedName, "type": "hosted", "protocol": "pypi",
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": proxyName, "type": "proxy", "protocol": "pypi",
		"upstream_url": env.upstream.URL + "/simple",
		"ttl_seconds":  3600, "negative_ttl_seconds": 300,
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": virtName, "type": "virtual", "protocol": "pypi",
		"members": mapped,
	})

	_, err := env.reg.Publish(t.Context(), registry.PublishParams{
		Repo: hostedName, Package: "our-internal-lib", Version: "1.0.0",
		Filename: hostedFile, Body: bytes.NewReader(hostedWheel), Size: int64(len(hostedWheel)),
		Immutable: true,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	before := env.requests.Load()
	idx := env.do(t, http.MethodGet, "/pypi/"+virtName+"/simple/our-internal-lib/", nil,
		"application/vnd.pypi.simple.v1+json")
	if idx.Code != http.StatusOK {
		t.Fatalf("index: %d %s", idx.Code, idx.Body.String())
	}
	if strings.Contains(idx.Body.String(), "99.0.0") {
		t.Fatalf("index leaked upstream version: %s", idx.Body.String())
	}
	if !strings.Contains(idx.Body.String(), "1.0.0") {
		t.Fatalf("index missing hosted version: %s", idx.Body.String())
	}
	dl := env.do(t, http.MethodGet, "/pypi/"+virtName+"/packages/our-internal-lib/"+hostedFile, nil, "")
	if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), hostedWheel) {
		t.Fatalf("download: %d", dl.Code)
	}
	// Missing hostile version must 404 without upstream contact.
	miss := env.do(t, http.MethodGet, "/pypi/"+virtName+"/packages/our-internal-lib/"+hostileFile, nil, "")
	if miss.Code != http.StatusNotFound {
		t.Fatalf("hostile file should 404, got %d", miss.Code)
	}
	if env.requests.Load() != before {
		t.Fatalf("dependency confusion contacted upstream: before=%d after=%d", before, env.requests.Load())
	}
}

func TestValidateRepository_LifecycleOnProxyFails(t *testing.T) {
	err := reconcile.ValidateRepository(reconcile.RepositorySpec{
		Name: "proxy/pypi-public", Type: model.RepoTypeProxy, Protocol: model.ProtocolPyPI,
		UpstreamURL: "https://example.com", LifecycleStages: []string{"prod"},
	})
	if err == nil {
		t.Fatal("expected validation failure")
	}
}

func TestProxy_NegativeCacheSeparateFromFiveHundred(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotFound)

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "appearing", "files": []any{},
			})
		}
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "proxy/neg", "type": "proxy", "protocol": "pypi",
		"upstream_url": env.upstream.URL + "/simple",
		"ttl_seconds":  3600, "negative_ttl_seconds": 3600,
	})

	miss := env.do(t, http.MethodGet, "/pypi/proxy/neg/simple/appearing/", nil,
		"application/vnd.pypi.simple.v1+json")
	if miss.Code != http.StatusNotFound {
		t.Fatalf("first 404: %d", miss.Code)
	}
	after404 := env.requests.Load()

	miss2 := env.do(t, http.MethodGet, "/pypi/proxy/neg/simple/appearing/", nil,
		"application/vnd.pypi.simple.v1+json")
	if miss2.Code != http.StatusNotFound {
		t.Fatalf("cached 404: %d", miss2.Code)
	}
	if env.requests.Load() != after404 {
		t.Fatal("fresh negative cache should not re-fetch")
	}

	// Separate proxy for 503 behaviour.
	status.Store(http.StatusServiceUnavailable)
	env2 := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	env2.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "proxy/five", "type": "proxy", "protocol": "pypi",
		"upstream_url": env2.upstream.URL + "/simple",
		"ttl_seconds":  3600, "negative_ttl_seconds": 3600,
	})
	first := env2.do(t, http.MethodGet, "/pypi/proxy/five/simple/x/", nil,
		"application/vnd.pypi.simple.v1+json")
	if first.Code == http.StatusNotFound {
		t.Fatal("503 must not become a cached 404")
	}
	n := env2.requests.Load()
	_ = env2.do(t, http.MethodGet, "/pypi/proxy/five/simple/x/", nil,
		"application/vnd.pypi.simple.v1+json")
	if env2.requests.Load() <= n {
		t.Fatal("503 must not write a negative cache entry")
	}
}

func TestProxyOCI_ManifestAndBlobThroughVirtual(t *testing.T) {
	layer := []byte("oci-layer")
	layerDigest := "sha256:" + shaHex(layer)
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := "sha256:" + shaHex(config)
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.Digest(configDigest),
			Size:      int64(len(config)),
		},
		Layers: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    digest.Digest(layerDigest),
			Size:      int64(len(layer)),
		}},
	}
	manBody, _ := json.Marshal(manifest)
	manDigest := "sha256:" + shaHex(manBody)

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", manDigest)
			_, _ = w.Write(manBody)
		case strings.Contains(r.URL.Path, "/blobs/"+layerDigest):
			_, _ = w.Write(layer)
		case strings.Contains(r.URL.Path, "/blobs/"+configDigest):
			_, _ = w.Write(config)
		default:
			http.NotFound(w, r)
		}
	})

	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "dockerhub", "type": "proxy", "protocol": "oci",
		"upstream_url": env.upstream.URL, "ttl_seconds": 3600, "negative_ttl_seconds": 300,
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "internal-oci", "type": "hosted", "protocol": "oci",
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "docker", "type": "virtual", "protocol": "oci",
		"members": []string{"internal-oci", "dockerhub"},
	})

	getMan := env.do(t, http.MethodGet, "/v2/dockerhub/library/alpine/manifests/latest", nil, "")
	if getMan.Code != http.StatusOK {
		t.Fatalf("proxy manifest: %d %s", getMan.Code, getMan.Body.String())
	}
	getBlob := env.do(t, http.MethodGet, "/v2/dockerhub/library/alpine/blobs/"+layerDigest, nil, "")
	if getBlob.Code != http.StatusOK || !bytes.Equal(getBlob.Body.Bytes(), layer) {
		t.Fatalf("proxy blob: %d", getBlob.Code)
	}

	// Cached blob must not be readable via a different repository (I2).
	cross := env.do(t, http.MethodGet, "/v2/internal-oci/blobs/"+layerDigest, nil, "")
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-repo blob should 404, got %d", cross.Code)
	}

	virt := env.do(t, http.MethodGet, "/v2/docker/library/alpine/manifests/latest", nil, "")
	if virt.Code != http.StatusOK {
		t.Fatalf("virtual manifest: %d %s", virt.Code, virt.Body.String())
	}

	// Write to proxy must fail.
	put := env.do(t, http.MethodPut, "/v2/dockerhub/library/alpine/manifests/evil", manBody, ocispec.MediaTypeImageManifest)
	if put.Code == http.StatusCreated {
		t.Fatal("proxy must reject pushes")
	}
}

// TestProxy_StaleIndexServedWhenUpstreamDown covers the watch-out that
// upstream failure must not poison the cache: once the TTL has expired, a
// failing upstream is asked, the stale index still answers, and the hit row
// survives the failure.
func TestProxy_StaleIndexServedWhenUpstreamDown(t *testing.T) {
	wheel := []byte("cached-bytes")
	filename := "cached_pkg-1.0.0-py3-none-any.whl"
	var fail atomic.Bool

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/simple/cached-pkg"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "cached-pkg",
				"files": []map[string]any{{
					"filename": filename,
					"url":      "/files/" + filename,
					"hashes":   map[string]string{"sha256": shaHex(wheel)},
					"size":     len(wheel),
				}},
			})
		case r.URL.Path == "/files/"+filename:
			_, _ = w.Write(wheel)
		default:
			http.NotFound(w, r)
		}
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "proxy/stale", "type": "proxy", "protocol": "pypi",
		"upstream_url": env.upstream.URL + "/simple",
		"ttl_seconds":  1, "negative_ttl_seconds": 1,
	})

	if idx := env.do(t, http.MethodGet, "/pypi/proxy/stale/simple/cached-pkg/", nil, "application/vnd.pypi.simple.v1+json"); idx.Code != http.StatusOK {
		t.Fatalf("warm index: %d", idx.Code)
	}
	if dl := env.do(t, http.MethodGet, "/pypi/proxy/stale/packages/cached-pkg/"+filename, nil, ""); dl.Code != http.StatusOK {
		t.Fatalf("warm download: %d", dl.Code)
	}

	fail.Store(true)
	waitPastTTL()
	before := env.requests.Load()

	idx := env.do(t, http.MethodGet, "/pypi/proxy/stale/simple/cached-pkg/", nil, "application/vnd.pypi.simple.v1+json")
	if idx.Code != http.StatusOK || !strings.Contains(idx.Body.String(), filename) {
		t.Fatalf("stale index with upstream down: %d %s", idx.Code, idx.Body.String())
	}
	if env.requests.Load() == before {
		t.Fatal("an expired entry must ask upstream before falling back")
	}
	entry, err := env.reg.GetCacheEntry(t.Context(), "proxy/stale", "pypi:index:cached-pkg")
	if err != nil {
		t.Fatalf("cache entry: %v", err)
	}
	if entry.Status != registry.CacheHit {
		t.Fatalf("a 502 replaced the hit: status %q", entry.Status)
	}

	dl := env.do(t, http.MethodGet, "/pypi/proxy/stale/packages/cached-pkg/"+filename, nil, "")
	if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), wheel) {
		t.Fatalf("stale file serve: %d", dl.Code)
	}
}

// TestProxy_BadDigestNotPlaced: upstream bytes that do not match the hash the
// index promised reach the client only as a cut connection, and are never
// placed.
func TestProxy_BadDigestNotPlaced(t *testing.T) {
	wheel := []byte("actual-bytes")
	filename := "bad_digest-1.0.0-py3-none-any.whl"
	wrongHex := shaHex([]byte("other"))

	env := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/simple/bad-digest"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "bad-digest",
				"files": []map[string]any{{
					"filename": filename,
					"url":      "/files/" + filename,
					"hashes":   map[string]string{"sha256": wrongHex},
					"size":     len(wheel),
				}},
			})
		case r.URL.Path == "/files/"+filename:
			_, _ = w.Write(wheel)
		default:
			http.NotFound(w, r)
		}
	})
	env.createJSON(t, "/api/v1/repositories", map[string]any{
		"name": "proxy/bad", "type": "proxy", "protocol": "pypi",
		"upstream_url": env.upstream.URL + "/simple",
		"ttl_seconds":  3600, "negative_ttl_seconds": 300,
	})
	srv := env.serve(t)

	if _, err := fetch(t, srv, "/pypi/proxy/bad/packages/bad-digest/"+filename); err == nil {
		t.Fatal("mismatched digest must not complete as a clean download")
	}
	files, err := env.reg.ListPackageFiles(t.Context(), "proxy/bad", "bad-digest")
	if err != nil && !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("list: %v", err)
	}
	if len(files) > 0 {
		t.Fatal("bad digest must not leave a files row")
	}
}
