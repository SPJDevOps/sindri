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
	client := newOCIClient(t)

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

	// A manifest naming a layer the registry does not hold, never pushed or
	// collected before the manifest arrived, gets the code that makes a
	// client push the layer again, not a 500.
	dangling := manifest
	dangling.Layers = []ocispec.Descriptor{{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.Digest(digestOf([]byte("never pushed"))),
		Size:      12,
	}}
	danglingBody, err := json.Marshal(dangling)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	unknown := mustDo(t, client, http.MethodPut, "/v2/demo/app/manifests/dangling",
		danglingBody, ocispec.MediaTypeImageManifest)
	if unknown.StatusCode != http.StatusBadRequest || !strings.Contains(unknown.Body, oci.CodeManifestBlobUnknown) {
		t.Fatalf("PUT manifest with an unknown layer: %d %s, want 400 %s",
			unknown.StatusCode, unknown.Body, oci.CodeManifestBlobUnknown)
	}
}

// newOCIClient serves the hosted OCI adapter, the management API and a real
// Postgres behind the server's router, and returns the handler.
func newOCIClient(t *testing.T) http.Handler {
	t.Helper()
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
			Auth:     auth.NewStaticBearer(testToken),
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv.Handler()
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
	// Every route needs a credential since M7: there is no anonymous pull.
	req.Header.Set("Authorization", "Bearer "+testToken)
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

// TestPushManifest_InvalidProvenanceIsRefused: a NUL in a provenance annotation
// or header is a client error in the OCI error shape, answered before anything
// is placed. The database cannot store a NUL, so before this it surfaced as a
// 500 at the provenance insert (SIN-63).
func TestPushManifest_InvalidProvenanceIsRefused(t *testing.T) {
	client := newOCIClient(t)
	createRepo(t, client, "demo/app", "oci")

	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := digestOf(config)
	pushBlob(t, client, "demo/app", config, configDigest)

	manifestWith := func(annotations map[string]string) []byte {
		body, err := json.Marshal(ocispec.Manifest{
			Versioned:   specs.Versioned{SchemaVersion: 2},
			MediaType:   ocispec.MediaTypeImageManifest,
			Config:      ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(configDigest), Size: int64(len(config))},
			Layers:      []ocispec.Descriptor{},
			Annotations: annotations,
		})
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		return body
	}

	tests := []struct {
		name    string
		body    []byte
		headers map[string]string
	}{
		{name: "NULInSourceAnnotation", body: manifestWith(map[string]string{ocispec.AnnotationSource: "a\x00b"})},
		{name: "NULInRevisionAnnotation", body: manifestWith(map[string]string{ocispec.AnnotationRevision: "a\x00b"})},
		{name: "NULInSourceHeader", body: manifestWith(nil), headers: map[string]string{"X-Sindri-Source-Ref": "a\x00b"}},
		{name: "NULInBuildHeader", body: manifestWith(nil), headers: map[string]string{"X-Sindri-Build-Ref": "a\x00b"}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tag := fmt.Sprintf("bad%d", i)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/v2/demo/app/manifests/"+tag, bytes.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
			for k, v := range tc.headers {
				req.Header[k] = []string{v}
			}
			rec := httptest.NewRecorder()
			client.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT manifest: %d %s, want 400", rec.Code, rec.Body)
			}
			var e struct {
				Errors []struct {
					Code string `json:"code"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || len(e.Errors) != 1 || e.Errors[0].Code != oci.CodeManifestInvalid {
				t.Fatalf("body = %s, want an OCI error body with code %s", rec.Body, oci.CodeManifestInvalid)
			}
			if got := mustDo(t, client, http.MethodGet, "/v2/demo/app/manifests/"+tag, nil, ""); got.StatusCode != http.StatusNotFound {
				t.Fatalf("GET refused manifest: %d %s, want 404 (nothing placed)", got.StatusCode, got.Body)
			}
		})
	}
	tags := mustDo(t, client, http.MethodGet, "/v2/demo/app/tags/list", nil, "")
	if strings.Contains(tags.Body, `"bad`) {
		t.Fatalf("tags after refusals = %s, want none placed", tags.Body)
	}
}

// TestOCI_TagsList_LargeNIsClampedWithLink: Distribution clamps an n above its
// maximum and pages with a Link header, and so does Sindri. Zero and negative
// n are still refused.
func TestOCI_TagsList_LargeNIsClampedWithLink(t *testing.T) {
	client := newOCIClient(t)
	createRepo(t, client, "demo/many", "oci")

	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := digestOf(config)
	pushBlob(t, client, "demo/many", config, configDigest)
	body, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(configDigest), Size: int64(len(config))},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	for i := 0; i < 201; i++ {
		put := mustDo(t, client, http.MethodPut, fmt.Sprintf("/v2/demo/many/manifests/t%03d", i), body, ocispec.MediaTypeImageManifest)
		if put.StatusCode != http.StatusCreated {
			t.Fatalf("PUT t%03d: %d %s", i, put.StatusCode, put.Body)
		}
	}

	tests := []struct {
		name     string
		query    string
		wantCode int
		wantTags int
		wantLink bool
	}{
		{"LargeNIsClampedToTheCapWithALink", "?n=1000", http.StatusOK, 200, true},
		{"NAtTheCapGetsALink", "?n=200", http.StatusOK, 200, true},
		{"SmallNIsHonoured", "?n=5", http.StatusOK, 5, true},
		{"ZeroNIsRefused", "?n=0", http.StatusBadRequest, 0, false},
		{"NegativeNIsRefused", "?n=-1", http.StatusBadRequest, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustDo(t, client, http.MethodGet, "/v2/demo/many/tags/list"+tc.query, nil, "")
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.wantCode, resp.Body)
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			var list struct {
				Tags []string `json:"tags"`
			}
			if err := json.Unmarshal([]byte(resp.Body), &list); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(list.Tags) != tc.wantTags {
				t.Fatalf("%d tags, want %d", len(list.Tags), tc.wantTags)
			}
			if got := resp.Header.Get("Link") != ""; got != tc.wantLink {
				t.Fatalf("Link present = %v, want %v", got, tc.wantLink)
			}
		})
	}
}

// pushImageManifest pushes a one-layer image under tag and returns the
// manifest's digest, size, and media type as a descriptor.
func pushImageManifest(t *testing.T, h http.Handler, repo, tag, seed string) ocispec.Descriptor {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","seed":"` + seed + `"}`)
	layer := []byte("layer for " + seed)
	pushBlob(t, h, repo, config, digestOf(config))
	pushBlob(t, h, repo, layer, digestOf(layer))
	body, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.Digest(digestOf(config)), Size: int64(len(config))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.Digest(digestOf(layer)), Size: int64(len(layer))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := mustDo(t, h, http.MethodPut, "/v2/"+repo+"/manifests/"+tag, body, ocispec.MediaTypeImageManifest)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("PUT subject manifest into %s: %d %s", repo, res.StatusCode, res.Body)
	}
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(digestOf(body)), Size: int64(len(body))}
}

// pushReferrerManifest pushes an artifact manifest (empty config) that names
// subject, by digest, and returns its descriptor and the PUT result. A
// non-empty explicitType is set as artifactType; otherwise the config media
// type decides.
func pushReferrerManifest(t *testing.T, h http.Handler, repo string, subject ocispec.Descriptor, explicitType, configType, seed string, annotations map[string]string) (ocispec.Descriptor, httpResult) {
	t.Helper()
	config := []byte(`{}`)
	pushBlob(t, h, repo, config, digestOf(config))
	layer := []byte("referrer payload " + seed)
	pushBlob(t, h, repo, layer, digestOf(layer))
	body, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: explicitType,
		Config:       ocispec.Descriptor{MediaType: configType, Digest: digest.Digest(digestOf(config)), Size: int64(len(config))},
		Layers:       []ocispec.Descriptor{{MediaType: "application/octet-stream", Digest: digest.Digest(digestOf(layer)), Size: int64(len(layer))}},
		Subject:      &subject,
		Annotations:  annotations,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := digestOf(body)
	res := mustDo(t, h, http.MethodPut, "/v2/"+repo+"/manifests/"+d, body, ocispec.MediaTypeImageManifest)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("PUT referrer into %s: %d %s", repo, res.StatusCode, res.Body)
	}
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(d), Size: int64(len(body))}, res
}

func listReferrers(t *testing.T, h http.Handler, repo, subject, query string) (ocispec.Index, httpResult) {
	t.Helper()
	path := "/v2/" + repo + "/referrers/" + subject
	if query != "" {
		path += "?" + query
	}
	res := mustDo(t, h, http.MethodGet, path, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, res.StatusCode, res.Body)
	}
	var idx ocispec.Index
	if err := json.Unmarshal([]byte(res.Body), &idx); err != nil {
		t.Fatalf("decoding referrers index %q: %v", res.Body, err)
	}
	return idx, res
}

func digestsOf(idx ocispec.Index) []string {
	out := make([]string, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		out = append(out, m.Digest.String())
	}
	return out
}

// TestOCI_ReferrersListsOnlyManifestsPlacedInTheRepository guards invariant
// I2 (ARCHITECTURE.md section 2) for the referrers API: a referrer is listed
// only in the repository it is placed in, even when another repository holds
// the same subject, and deleting it by digest removes the entry.
func TestOCI_ReferrersListsOnlyManifestsPlacedInTheRepository(t *testing.T) {
	client := newOCIClient(t)
	createRepo(t, client, "team-a/app", "oci")
	createRepo(t, client, "team-b/app", "oci")

	subjectA := pushImageManifest(t, client, "team-a/app", "1.0", "same")
	subjectB := pushImageManifest(t, client, "team-b/app", "1.0", "same")
	if subjectA.Digest != subjectB.Digest {
		t.Fatalf("setup: subjects differ: %s vs %s", subjectA.Digest, subjectB.Digest)
	}
	referrer, _ := pushReferrerManifest(t, client, "team-a/app", subjectA, "application/vnd.example.sig", ocispec.MediaTypeEmptyJSON, "sig", nil)

	inA, _ := listReferrers(t, client, "team-a/app", subjectA.Digest.String(), "")
	if got := digestsOf(inA); len(got) != 1 || got[0] != referrer.Digest.String() {
		t.Fatalf("team-a list = %v, want [%s]", got, referrer.Digest)
	}
	inB, _ := listReferrers(t, client, "team-b/app", subjectA.Digest.String(), "")
	if got := digestsOf(inB); len(got) != 0 {
		t.Fatalf("team-b list = %v, want empty: a referrer placed only in team-a leaked", got)
	}

	del := mustDo(t, client, http.MethodDelete, "/v2/team-a/app/manifests/"+referrer.Digest.String(), nil, "")
	if del.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE referrer: %d %s", del.StatusCode, del.Body)
	}
	after, _ := listReferrers(t, client, "team-a/app", subjectA.Digest.String(), "")
	if got := digestsOf(after); len(got) != 0 {
		t.Fatalf("team-a list after delete = %v, want empty", got)
	}
}

func TestOCI_ReferrersFilterAndSubjectHeader(t *testing.T) {
	client := newOCIClient(t)
	createRepo(t, client, "demo/app", "oci")
	subject := pushImageManifest(t, client, "demo/app", "1.0", "filter")

	annotations := map[string]string{"org.example.note": "hello"}
	sig, sigPut := pushReferrerManifest(t, client, "demo/app", subject, "application/vnd.example.sig", ocispec.MediaTypeEmptyJSON, "sig", annotations)
	sbom, _ := pushReferrerManifest(t, client, "demo/app", subject, "", "application/vnd.example.sbom.config", "sbom", nil)

	// An index with a subject is a referrer too.
	idxBody, err := json.Marshal(ocispec.Index{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageIndex,
		ArtifactType: "application/vnd.example.bundle",
		Manifests:    []ocispec.Descriptor{sig},
		Subject:      &subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	idxDigest := digestOf(idxBody)
	idxPut := mustDo(t, client, http.MethodPut, "/v2/demo/app/manifests/"+idxDigest, idxBody, ocispec.MediaTypeImageIndex)
	if idxPut.StatusCode != http.StatusCreated {
		t.Fatalf("PUT index with subject: %d %s", idxPut.StatusCode, idxPut.Body)
	}

	for _, put := range []httpResult{sigPut, idxPut} {
		if got := put.Header.Get("OCI-Subject"); got != subject.Digest.String() {
			t.Fatalf("OCI-Subject = %q, want %s", got, subject.Digest)
		}
	}
	plain := mustDo(t, client, http.MethodPut, "/v2/demo/app/manifests/2.0", []byte(mustDo(t, client, http.MethodGet, "/v2/demo/app/manifests/1.0", nil, "").Body), ocispec.MediaTypeImageManifest)
	if plain.StatusCode != http.StatusCreated || plain.Header.Get("OCI-Subject") != "" {
		t.Fatalf("push without subject: %d, OCI-Subject %q, want 201 and none", plain.StatusCode, plain.Header.Get("OCI-Subject"))
	}

	cases := []struct {
		name       string
		query      string
		want       []string
		wantFilter bool
	}{
		{name: "no filter lists all three", query: "", want: []string{sig.Digest.String(), sbom.Digest.String(), idxDigest}},
		{name: "explicit artifactType filters", query: "artifactType=application/vnd.example.sig", want: []string{sig.Digest.String()}, wantFilter: true},
		{name: "config media type is the fallback artifactType", query: "artifactType=application/vnd.example.sbom.config", want: []string{sbom.Digest.String()}, wantFilter: true},
		{name: "index artifactType filters", query: "artifactType=application/vnd.example.bundle", want: []string{idxDigest}, wantFilter: true},
		{name: "unmatched filter lists none", query: "artifactType=application/none", want: nil, wantFilter: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, res := listReferrers(t, client, "demo/app", subject.Digest.String(), tc.query)
			got := digestsOf(idx)
			if !sameSet(got, tc.want) {
				t.Fatalf("listed %v, want %v", got, tc.want)
			}
			if ct := res.Header.Get("Content-Type"); ct != ocispec.MediaTypeImageIndex {
				t.Fatalf("Content-Type = %q", ct)
			}
			if idx.SchemaVersion != 2 || idx.MediaType != ocispec.MediaTypeImageIndex {
				t.Fatalf("index envelope = %+v", idx)
			}
			if applied := res.Header.Get("OCI-Filters-Applied") == "artifactType"; applied != tc.wantFilter {
				t.Fatalf("OCI-Filters-Applied = %q, want applied=%v", res.Header.Get("OCI-Filters-Applied"), tc.wantFilter)
			}
		})
	}

	idx, _ := listReferrers(t, client, "demo/app", subject.Digest.String(), "")
	byDigest := map[string]ocispec.Descriptor{}
	for _, m := range idx.Manifests {
		byDigest[m.Digest.String()] = m
	}
	if d := byDigest[sig.Digest.String()]; d.Size != sig.Size || d.MediaType != ocispec.MediaTypeImageManifest ||
		d.ArtifactType != "application/vnd.example.sig" || d.Annotations["org.example.note"] != "hello" {
		t.Fatalf("sig descriptor = %+v, want size %d, annotations %v", d, sig.Size, annotations)
	}
	if d := byDigest[sbom.Digest.String()]; d.ArtifactType != "application/vnd.example.sbom.config" || len(d.Annotations) != 0 {
		t.Fatalf("sbom descriptor = %+v", d)
	}
	if d := byDigest[idxDigest]; d.MediaType != ocispec.MediaTypeImageIndex || d.Size != int64(len(idxBody)) {
		t.Fatalf("index descriptor = %+v", d)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestOCI_ReferrersOfUnknownSubjectIsAnEmptyIndex(t *testing.T) {
	client := newOCIClient(t)
	createRepo(t, client, "demo/app", "oci")

	unknown := digestOf([]byte("never pushed"))
	res := mustDo(t, client, http.MethodGet, "/v2/demo/app/referrers/"+unknown, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unknown subject: %d %s, want 200", res.StatusCode, res.Body)
	}
	if ct := res.Header.Get("Content-Type"); ct != ocispec.MediaTypeImageIndex {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(res.Body, `"manifests":[]`) {
		t.Fatalf("body %s lacks an empty manifests array", res.Body)
	}

	for _, bad := range []string{"sha256:short", "nonsense", "sha256:" + strings.Repeat("Z", 64)} {
		t.Run("malformed "+bad[:min(len(bad), 12)], func(t *testing.T) {
			res := mustDo(t, client, http.MethodGet, "/v2/demo/app/referrers/"+bad, nil, "")
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d %s, want 400", res.StatusCode, res.Body)
			}
			var e struct {
				Errors []struct {
					Code string `json:"code"`
				} `json:"errors"`
			}
			if err := json.Unmarshal([]byte(res.Body), &e); err != nil || len(e.Errors) == 0 || e.Errors[0].Code != oci.CodeDigestInvalid {
				t.Fatalf("body %s is not an OCI %s error", res.Body, oci.CodeDigestInvalid)
			}
		})
	}
}

func TestOCI_ReferrersPagesWithLinkHeader(t *testing.T) {
	defer oci.SetReferrersPageSize(2)()
	client := newOCIClient(t)
	createRepo(t, client, "demo/app", "oci")
	subject := pushImageManifest(t, client, "demo/app", "1.0", "paging")

	want := map[string]bool{}
	for i := range 5 {
		d, _ := pushReferrerManifest(t, client, "demo/app", subject, "application/vnd.example.sig", ocispec.MediaTypeEmptyJSON, fmt.Sprintf("r%d", i), nil)
		want[d.Digest.String()] = true
	}

	seen := map[string]int{}
	path := "/v2/demo/app/referrers/" + subject.Digest.String() + "?artifactType=application%2Fvnd.example.sig"
	pages := 0
	for path != "" {
		pages++
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}
		res := mustDo(t, client, http.MethodGet, path, nil, "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, res.StatusCode, res.Body)
		}
		if res.Header.Get("OCI-Filters-Applied") != "artifactType" {
			t.Fatalf("page %d lacks OCI-Filters-Applied", pages)
		}
		var idx ocispec.Index
		if err := json.Unmarshal([]byte(res.Body), &idx); err != nil {
			t.Fatal(err)
		}
		for _, d := range digestsOf(idx) {
			seen[d]++
		}
		path = ""
		if link := res.Header.Get("Link"); link != "" {
			start, end := strings.Index(link, "<"), strings.Index(link, ">")
			if start != 0 || end < 0 || !strings.HasSuffix(link, `; rel="next"`) {
				t.Fatalf("Link = %q", link)
			}
			path = link[1:end]
			if !strings.Contains(path, "artifactType=application%2Fvnd.example.sig") {
				t.Fatalf("next link %q dropped the filter", path)
			}
		}
	}
	if pages != 3 || len(seen) != len(want) {
		t.Fatalf("pages = %d, distinct = %d, want 3 and %d", pages, len(seen), len(want))
	}
	for d, n := range seen {
		if !want[d] || n != 1 {
			t.Fatalf("referrer %s seen %d times (expected: %v)", d, n, want[d])
		}
	}
}

// An image whose name has a referrers segment keeps working: the route is
// recognised only as the last two segments, /referrers/<digest> (ADR 32).
func TestOCI_ImageNameWithReferrersSegmentStillServesManifests(t *testing.T) {
	client := newOCIClient(t)
	createRepo(t, client, "team", "oci")

	subject := pushImageManifest(t, client, "team/referrers/app", "1.0", "legacy")
	got := mustDo(t, client, http.MethodGet, "/v2/team/referrers/app/manifests/1.0", nil, "")
	if got.StatusCode != http.StatusOK || digestOf([]byte(got.Body)) != subject.Digest.String() {
		t.Fatalf("GET manifest of team/referrers/app: %d %s", got.StatusCode, got.Body)
	}
	tags := mustDo(t, client, http.MethodGet, "/v2/team/referrers/app/tags/list", nil, "")
	if tags.StatusCode != http.StatusOK || !strings.Contains(tags.Body, `"1.0"`) {
		t.Fatalf("tags of team/referrers/app: %d %s", tags.StatusCode, tags.Body)
	}
	idx, _ := listReferrers(t, client, "team", subject.Digest.String(), "")
	if len(idx.Manifests) != 0 {
		t.Fatalf("referrers = %v, want none", digestsOf(idx))
	}
}
