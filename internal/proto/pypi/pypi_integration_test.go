//go:build integration

package pypi_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/proto/pypi"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

const testToken = "m4-e2e-token"

// TestPyPI_UploadInstallAndPlacementGate exercises the hosted PyPI adapter:
// create repos via the management API, twine-shaped multipart upload, simple
// HTML and JSON indexes, byte-identical download, and cross-repo 404 (I2).
func TestPyPI_UploadInstallAndPlacementGate(t *testing.T) {
	client := newTestHandler(t)

	createRepo(t, client, "internal", "pypi")
	createRepo(t, client, "other", "pypi")

	wheel := buildWheel(t, "demo-package", "1.0.0", ">=3.9")
	filename := "demo_package-1.0.0-py3-none-any.whl"
	sum := sha256.Sum256(wheel)
	shaHex := hex.EncodeToString(sum[:])

	upload := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
		":action":       "file_upload",
		"name":          "Demo.Package",
		"version":       "1.0.0",
		"sha256_digest": shaHex,
	}, filename, wheel)
	if upload.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d %s", upload.StatusCode, upload.Body)
	}

	// Non-normalized project name redirects.
	redir := mustDo(t, client, http.MethodGet, "/pypi/internal/simple/Demo.Package/", nil, "", "")
	if redir.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("normalize redirect: %d", redir.StatusCode)
	}
	if loc := redir.Header.Get("Location"); !strings.HasSuffix(loc, "/pypi/internal/simple/demo-package/") {
		t.Fatalf("Location = %q", loc)
	}

	html := mustDo(t, client, http.MethodGet, "/pypi/internal/simple/demo-package/", nil, "", "")
	if html.StatusCode != http.StatusOK {
		t.Fatalf("simple HTML: %d %s", html.StatusCode, html.Body)
	}
	if !strings.Contains(html.Body, filename) {
		t.Fatalf("simple HTML missing filename: %s", html.Body)
	}
	if !strings.Contains(html.Body, "#sha256="+shaHex) {
		t.Fatalf("simple HTML missing hash fragment: %s", html.Body)
	}
	if !strings.Contains(html.Body, `data-requires-python="&gt;=3.9"`) &&
		!strings.Contains(html.Body, `data-requires-python=">=3.9"`) {
		t.Fatalf("simple HTML missing requires-python: %s", html.Body)
	}

	js := mustDo(t, client, http.MethodGet, "/pypi/internal/simple/demo-package/", nil, "",
		"application/vnd.pypi.simple.v1+json")
	if js.StatusCode != http.StatusOK {
		t.Fatalf("simple JSON: %d %s", js.StatusCode, js.Body)
	}
	var index struct {
		Name  string `json:"name"`
		Files []struct {
			Filename string            `json:"filename"`
			Hashes   map[string]string `json:"hashes"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(js.Body), &index); err != nil {
		t.Fatalf("decode JSON index: %v\n%s", err, js.Body)
	}
	if index.Name != "demo-package" || len(index.Files) != 1 {
		t.Fatalf("JSON index = %+v", index)
	}
	if index.Files[0].Hashes["sha256"] != shaHex {
		t.Fatalf("JSON hash = %q", index.Files[0].Hashes["sha256"])
	}

	dl := mustDo(t, client, http.MethodGet,
		"/pypi/internal/packages/demo-package/"+filename, nil, "", "")
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", dl.StatusCode, dl.Body)
	}
	if dl.Body != string(wheel) {
		t.Fatalf("download body mismatch: got %d bytes, want %d", len(dl.Body), len(wheel))
	}
	if got, want := dl.Header.Get("Content-Length"), strconv.Itoa(len(wheel)); got != want {
		t.Fatalf("download Content-Length = %q, want %q", got, want)
	}
	if got := dl.Header.Get("Content-Type"); got != "application/zip" {
		t.Fatalf("download Content-Type = %q", got)
	}

	head := mustDo(t, client, http.MethodHead,
		"/pypi/internal/packages/demo-package/"+filename, nil, "", "")
	if head.StatusCode != http.StatusOK || head.Body != "" {
		t.Fatalf("HEAD download = %d with %d body bytes", head.StatusCode, len(head.Body))
	}
	if got, want := head.Header.Get("Content-Length"), strconv.Itoa(len(wheel)); got != want {
		t.Fatalf("HEAD Content-Length = %q, want %q", got, want)
	}

	// Invariant I2: same filename under another repository is unknown.
	cross := mustDo(t, client, http.MethodGet,
		"/pypi/other/packages/demo-package/"+filename, nil, "", "")
	if cross.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repo GET status = %d, want 404; body %s", cross.StatusCode, cross.Body)
	}

	root := mustDo(t, client, http.MethodGet, "/pypi/internal/simple/", nil, "", "")
	if root.StatusCode != http.StatusOK {
		t.Fatalf("root simple: %d %s", root.StatusCode, root.Body)
	}
	if !strings.Contains(root.Body, "demo-package") {
		t.Fatalf("root simple missing project: %s", root.Body)
	}
}

func buildWheel(t *testing.T, name, version, requiresPython string) []byte {
	t.Helper()
	meta := "Name: " + name + "\nVersion: " + version + "\nRequires-Python: " + requiresPython + "\n"
	distInfo := strings.ReplaceAll(name, "-", "_") + "-" + version + ".dist-info/METADATA"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(distInfo)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte(meta)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Minimal pure-python wheel payload so the archive is non-empty.
	pw, err := zw.Create(strings.ReplaceAll(name, "-", "_") + "/__init__.py")
	if err != nil {
		t.Fatalf("Create init: %v", err)
	}
	if _, err := pw.Write([]byte("__version__ = \"" + version + "\"\n")); err != nil {
		t.Fatalf("Write init: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func multipartUpload(t *testing.T, h http.Handler, path string, fields map[string]string, filename string, content []byte) httpResult {
	t.Helper()
	return doMultipart(t, h, path, fields, filename, content, true)
}

func doMultipart(t *testing.T, h http.Handler, path string, fields map[string]string, filename string, content []byte, withBasic bool) httpResult {
	t.Helper()
	return doMultipartWithHeaders(t, h, path, fields, filename, content, withBasic, nil)
}

func doMultipartWithHeaders(t *testing.T, h http.Handler, path string, fields map[string]string, filename string, content []byte, withBasic bool, headers map[string]string) httpResult {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("WriteField %s: %v", k, err)
		}
	}
	part, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("Write content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close multipart: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if withBasic {
		req.SetBasicAuth("token", "anything")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return httpResult{StatusCode: rec.Code, Body: rec.Body.String(), Header: rec.Header()}
}

func createRepo(t *testing.T, h http.Handler, name, protocol string) {
	t.Helper()
	body := `{"name":"` + name + `","type":"hosted","protocol":"` + protocol + `"}`
	res := mustDo(t, h, http.MethodPost, "/api/v1/repositories", []byte(body), "application/json", "")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create repo %s: %d %s", name, res.StatusCode, res.Body)
	}
}

type httpResult struct {
	StatusCode int
	Body       string
	Header     http.Header
}

func mustDo(t *testing.T, h http.Handler, method, path string, body []byte, contentType, accept string) httpResult {
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
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return httpResult{
		StatusCode: rec.Code,
		Body:       rec.Body.String(),
		Header:     rec.Header(),
	}
}

// newTestHandler wires the real server around a real Postgres and an in-memory
// blob store, which runs the same storage conformance suite as the backends.
func newTestHandler(t *testing.T) http.Handler {
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

	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		API: api.Handler(api.Options{
			Registry:  reg,
			Lifecycle: lifecycle.New(database),
			Evidence:  evidence.New(database, casStore, "memory"),
			Auth:      auth.NewStaticBearer(testToken),
			RequestID: server.RequestIDFromContext,
		}),
		PyPI:   pypi.Handler(pypi.Options{Resolver: resolver}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv.Handler()
}
