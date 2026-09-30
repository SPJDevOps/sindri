//go:build integration

package oci_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/proto/oci"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const testToken = "m3-e2e-token"

// TestOCI_PushPullAndPlacementGate exercises the hosted OCI adapter over HTTP:
// create repos via the management API, push a blob and manifest, pull them
// back byte-identical, and fail a cross-repo blob GET with 404 (invariant I2).
func TestOCI_PushPullAndPlacementGate(t *testing.T) {
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
	remoteClient := remote.New(remote.Options{Registry: reg})
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
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	client := srv.Handler()

	createRepo(t, client, "demo/app", "oci")
	createRepo(t, client, "other/app", "oci")

	// Version check.
	v2 := mustDo(t, client, http.MethodGet, "/v2/", nil, "")
	if v2.StatusCode != http.StatusOK {
		t.Fatalf("GET /v2/: %d", v2.StatusCode)
	}
	if v2.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("API version header = %q", v2.Header.Get("Docker-Distribution-API-Version"))
	}

	layer := []byte("layer-bytes-for-m3")
	layerDigest := digestOf(layer)
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := digestOf(config)

	pushBlob(t, client, "demo/app", layer, layerDigest)
	pushBlob(t, client, "demo/app", config, configDigest)

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
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	putMan := mustDo(t, client, http.MethodPut, "/v2/demo/app/manifests/1.0",
		body, ocispec.MediaTypeImageManifest)
	if putMan.StatusCode != http.StatusCreated {
		t.Fatalf("PUT manifest: %d %s", putMan.StatusCode, putMan.Body)
	}
	manifestDigest := putMan.Header.Get("Docker-Content-Digest")
	if manifestDigest == "" {
		t.Fatal("missing Docker-Content-Digest on manifest put")
	}

	gotMan := mustDo(t, client, http.MethodGet, "/v2/demo/app/manifests/1.0", nil, "")
	if gotMan.StatusCode != http.StatusOK {
		t.Fatalf("GET manifest: %d %s", gotMan.StatusCode, gotMan.Body)
	}
	if gotMan.Body != string(body) {
		t.Fatalf("manifest body mismatch")
	}

	gotLayer := mustDo(t, client, http.MethodGet, "/v2/demo/app/blobs/"+layerDigest, nil, "")
	if gotLayer.StatusCode != http.StatusOK {
		t.Fatalf("GET blob: %d %s", gotLayer.StatusCode, gotLayer.Body)
	}
	if gotLayer.Body != string(layer) {
		t.Fatalf("layer body = %q, want %q", gotLayer.Body, layer)
	}

	// Invariant I2: same digest under another repository is unknown.
	cross := mustDo(t, client, http.MethodGet, "/v2/other/app/blobs/"+layerDigest, nil, "")
	if cross.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repo GET status = %d, want 404; body %s", cross.StatusCode, cross.Body)
	}
	if !strings.Contains(cross.Body, oci.CodeBlobUnknown) && !strings.Contains(cross.Body, "BLOB_UNKNOWN") {
		t.Fatalf("cross-repo error body = %s, want BLOB_UNKNOWN", cross.Body)
	}

	tags := mustDo(t, client, http.MethodGet, "/v2/demo/app/tags/list", nil, "")
	if tags.StatusCode != http.StatusOK {
		t.Fatalf("tags/list: %d %s", tags.StatusCode, tags.Body)
	}
	if !strings.Contains(tags.Body, `"1.0"`) {
		t.Fatalf("tags list missing 1.0: %s", tags.Body)
	}

	// Chunked upload round-trip.
	chunkPayload := []byte("chunk-one-chunk-two")
	chunkDigest := digestOf(chunkPayload)
	start := mustDo(t, client, http.MethodPost, "/v2/demo/app/blobs/uploads/", nil, "")
	if start.StatusCode != http.StatusAccepted {
		t.Fatalf("POST upload: %d %s", start.StatusCode, start.Body)
	}
	loc := start.Header.Get("Location")
	if loc == "" {
		t.Fatal("upload start missing Location")
	}
	patch := mustDo(t, client, http.MethodPatch, loc, chunkPayload[:9], "application/octet-stream")
	if patch.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH upload: %d %s", patch.StatusCode, patch.Body)
	}
	put := mustDo(t, client, http.MethodPut, loc+"?digest="+chunkDigest, chunkPayload[9:], "application/octet-stream")
	if put.StatusCode != http.StatusCreated {
		t.Fatalf("PUT upload: %d %s", put.StatusCode, put.Body)
	}
	gotChunk := mustDo(t, client, http.MethodGet, "/v2/demo/app/blobs/"+chunkDigest, nil, "")
	if gotChunk.StatusCode != http.StatusOK || gotChunk.Body != string(chunkPayload) {
		t.Fatalf("chunked blob GET: %d %q", gotChunk.StatusCode, gotChunk.Body)
	}
}

func pushBlob(t *testing.T, h http.Handler, repo string, payload []byte, digest string) {
	t.Helper()
	path := fmt.Sprintf("/v2/%s/blobs/uploads/?digest=%s", repo, digest)
	res := mustDo(t, h, http.MethodPost, path, payload, "application/octet-stream")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("monolithic upload to %s: %d %s", repo, res.StatusCode, res.Body)
	}
}

func createRepo(t *testing.T, h http.Handler, name, protocol string) {
	t.Helper()
	body := `{"name":"` + name + `","type":"hosted","protocol":"` + protocol + `"}`
	res := mustDo(t, h, http.MethodPost, "/api/v1/repositories", []byte(body), "application/json")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create repo %s: %d %s", name, res.StatusCode, res.Body)
	}
}

type httpResult struct {
	StatusCode int
	Body       string
	Header     http.Header
}

func mustDo(t *testing.T, h http.Handler, method, path string, body []byte, contentType string) httpResult {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rdr)
	if strings.HasPrefix(path, "/api/") {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return httpResult{
		StatusCode: rec.Code,
		Body:       rec.Body.String(),
		Header:     rec.Header(),
	}
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
