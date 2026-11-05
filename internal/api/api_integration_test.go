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

func TestPublish_InvalidProvenanceHeaderIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{name: "InvalidUTF8InSourceRef", header: model.HeaderSourceRef, value: "git\xff"},
		{name: "NULInSourceRef", header: model.HeaderSourceRef, value: "git\x00"},
		{name: "InvalidUTF8InBuildRef", header: model.HeaderBuildRef, value: "ci\xc3"},
		{name: "NULInBuildRef", header: model.HeaderBuildRef, value: "ci\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := testsupport.MigratedDB(t)
			blobs := memstore.New()
			reg := registry.New(database, cas.New(blobs, cas.WithReserver(db.NewBlobReserver(database, "memory"))), "memory")
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
			client := srv.Handler()
			createRepo(t, client, "team-a", "oci")

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut,
				"/api/v1/repositories/team-a/packages/demo/versions/1.0.0/files/demo.txt",
				strings.NewReader("payload"))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", "text/plain")
			req.Header[tc.header] = []string{tc.value}
			rec := httptest.NewRecorder()
			client.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}

			var files, stored int
			if err := database.Pool().QueryRow(t.Context(), `SELECT count(*) FROM files`).Scan(&files); err != nil {
				t.Fatalf("counting files: %v", err)
			}
			if err := database.Pool().QueryRow(t.Context(), `SELECT count(*) FROM blobs`).Scan(&stored); err != nil {
				t.Fatalf("counting blobs: %v", err)
			}
			if files != 0 || stored != 0 {
				t.Fatalf("after the refusal: %d file rows and %d blobs, want none", files, stored)
			}
		})
	}
}

// A published version or filename is immutable (SIN-52), but only for the
// protocols whose clients resolve by it. A hosted OCI-protocol repository keeps
// the v1.0 behaviour: its API files can be deleted and put again.
func TestAPI_RepublishAfterDelete(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		want     int
	}{
		{name: "PyPIFilenameIsRefusedWith409", protocol: "pypi", want: http.StatusConflict},
		{name: "OCIProtocolFileCanBePublishedAgain", protocol: "oci", want: http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := testsupport.MigratedDB(t)
			blobs := memstore.New()
			reg := registry.New(database, cas.New(blobs, cas.WithReserver(db.NewBlobReserver(database, "memory"))), "memory")
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
			client := srv.Handler()
			createRepo(t, client, "team-a", tc.protocol)
			const path = "/api/v1/repositories/team-a/packages/demo/versions/1.0.0/files/demo-1.0.0.tar.gz"

			if res := mustDo(t, client, http.MethodPut, path, []byte("first"), "text/plain"); res.StatusCode != http.StatusCreated {
				t.Fatalf("first PUT = %d %s, want 201", res.StatusCode, res.Body)
			}
			if res := mustDo(t, client, http.MethodDelete, path, nil, ""); res.StatusCode != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s, want 204", res.StatusCode, res.Body)
			}

			res := mustDo(t, client, http.MethodPut, path, []byte("second"), "text/plain")
			if res.StatusCode != tc.want {
				t.Fatalf("second PUT = %d %s, want %d", res.StatusCode, res.Body, tc.want)
			}
			if tc.want == http.StatusConflict && !strings.Contains(res.Body, `"code":"FILE_EXISTS"`) {
				t.Fatalf("conflict body = %s, want code FILE_EXISTS", res.Body)
			}
		})
	}
}
