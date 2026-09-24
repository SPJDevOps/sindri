//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const testToken = "m2-e2e-token"

// TestAPI_PlacementGatesCrossRepoReads is the M2 gate: over HTTP only, create a
// hosted repository, upload a blob, list it, read it back byte-identical, fail
// to read it via a different repository's path with 404 (invariant I2), and
// delete it.
func TestAPI_PlacementGatesCrossRepoReads(t *testing.T) {
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
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	client := srv.Handler()

	payload := []byte("hello from m2 placement e2e")

	// 1. Create hosted repository A.
	createRepo(t, client, "team-a", "oci")
	// Create repository B so the cross-repo path exists as a namespace.
	createRepo(t, client, "team-b", "oci")

	// 2. Upload a blob into A.
	put := mustDo(t, client, http.MethodPut,
		"/api/v1/repositories/team-a/packages/demo/versions/1.0.0/files/demo.txt",
		payload, "text/plain")
	if put.StatusCode != http.StatusCreated {
		t.Fatalf("PUT file: status %d body %s", put.StatusCode, put.Body)
	}
	var published struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(put.Body), &published); err != nil {
		t.Fatalf("decoding publish response: %v", err)
	}
	if published.Digest == "" || published.Size != int64(len(payload)) {
		t.Fatalf("publish response = %+v", published)
	}

	// 3. List packages and versions.
	listPkgs := mustDo(t, client, http.MethodGet, "/api/v1/repositories/team-a/packages", nil, "")
	if listPkgs.StatusCode != http.StatusOK {
		t.Fatalf("list packages: %d %s", listPkgs.StatusCode, listPkgs.Body)
	}
	if !strings.Contains(listPkgs.Body, `"demo"`) {
		t.Fatalf("package list missing demo: %s", listPkgs.Body)
	}

	listVers := mustDo(t, client, http.MethodGet,
		"/api/v1/repositories/team-a/packages/demo/versions", nil, "")
	if listVers.StatusCode != http.StatusOK {
		t.Fatalf("list versions: %d %s", listVers.StatusCode, listVers.Body)
	}
	if !strings.Contains(listVers.Body, `"1.0.0"`) {
		t.Fatalf("version list missing 1.0.0: %s", listVers.Body)
	}

	// 4. Read it back byte-identical.
	got := mustDo(t, client, http.MethodGet,
		"/api/v1/repositories/team-a/packages/demo/versions/1.0.0/files/demo.txt",
		nil, "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET file: %d %s", got.StatusCode, got.Body)
	}
	if got.Body != string(payload) {
		t.Fatalf("GET body = %q, want %q", got.Body, payload)
	}

	// 5. Invariant I2: the same digest path under another repository is 404.
	cross := mustDo(t, client, http.MethodGet,
		"/api/v1/repositories/team-b/packages/demo/versions/1.0.0/files/demo.txt",
		nil, "")
	if cross.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repo GET status = %d, want 404 (I2); body %s", cross.StatusCode, cross.Body)
	}
	var errBody api.ErrorBody
	if err := json.Unmarshal([]byte(cross.Body), &errBody); err != nil {
		t.Fatalf("cross-repo error body: %v", err)
	}
	if errBody.Code != "NOT_FOUND" {
		t.Fatalf("cross-repo error code = %q, want NOT_FOUND", errBody.Code)
	}

	// 6. Delete the placement; subsequent GET is 404. Bytes must not be
	// reclaimed here (no cas.Delete from the API).
	del := mustDo(t, client, http.MethodDelete,
		"/api/v1/repositories/team-a/packages/demo/versions/1.0.0/files/demo.txt",
		nil, "")
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE file: %d %s", del.StatusCode, del.Body)
	}
	after := mustDo(t, client, http.MethodGet,
		"/api/v1/repositories/team-a/packages/demo/versions/1.0.0/files/demo.txt",
		nil, "")
	if after.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete: %d, want 404", after.StatusCode)
	}

	digest, err := model.ParseDigest(published.Digest)
	if err != nil {
		t.Fatalf("ParseDigest(%q): %v", published.Digest, err)
	}
	if _, err := blobs.Stat(t.Context(), digest); err != nil {
		t.Fatalf("blob missing from CAS after placement delete (must not cas.Delete): %v", err)
	}
}

func TestAPI_RequiresBearerToken(t *testing.T) {
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
	reg := registry.New(database, cas.New(blobs), "memory")
	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		API: api.Handler(api.Options{
			Registry:  reg,
			Auth:      auth.NewStaticBearer(testToken),
			RequestID: server.RequestIDFromContext,
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/repositories", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
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
