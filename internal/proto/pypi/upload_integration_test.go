//go:build integration

package pypi_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPyPI_UploadValidationAndImmutability covers the write-time rules: an
// artifact must agree with the name and version it is published as, a claimed
// hash is checked against the bytes, and published bytes never change under a
// consumer that pinned their hash.
func TestPyPI_UploadValidationAndImmutability(t *testing.T) {
	client := newTestHandler(t)
	createRepo(t, client, "internal", "pypi")
	createRepo(t, client, "images", "oci")

	t.Run("rejects an upload with no credentials", func(t *testing.T) {
		wheel := buildWheel(t, "anon-demo", "1.0.0", ">=3.9")
		res := doMultipart(t, client, "/pypi/internal/upload", map[string]string{
			"name": "anon-demo", "version": "1.0.0",
		}, "anon_demo-1.0.0-py3-none-any.whl", wheel, false)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body %s", res.StatusCode, res.Body)
		}
		if res.Header.Get("WWW-Authenticate") == "" {
			t.Fatal("401 without a WWW-Authenticate challenge")
		}
	})

	t.Run("rejects a claimed digest that is not the bytes", func(t *testing.T) {
		wheel := buildWheel(t, "claim-demo", "1.0.0", ">=3.9")
		other := sha256.Sum256([]byte("not these bytes"))
		res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "claim-demo", "version": "1.0.0",
			"sha256_digest": hex.EncodeToString(other[:]),
		}, "claim_demo-1.0.0-py3-none-any.whl", wheel)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", res.StatusCode, res.Body)
		}
		assertProjectAbsent(t, client, "claim-demo")
	})

	t.Run("rejects a version the artifact does not claim", func(t *testing.T) {
		wheel := buildWheel(t, "inflate", "1.0.0", ">=3.9")
		res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "inflate", "version": "9.9.9",
		}, "inflate-1.0.0-py3-none-any.whl", wheel)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", res.StatusCode, res.Body)
		}
		assertProjectAbsent(t, client, "inflate")
	})

	t.Run("rejects an artifact belonging to another project", func(t *testing.T) {
		wheel := buildWheel(t, "evil", "1.0.0", ">=3.9")
		res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "trusted", "version": "1.0.0",
		}, "evil-1.0.0-py3-none-any.whl", wheel)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", res.StatusCode, res.Body)
		}
		assertProjectAbsent(t, client, "trusted")
	})

	// mime/multipart applies filepath.Base to a part filename, so a separator
	// never reaches the handler over HTTP. These are the shapes that do.
	t.Run("rejects a filename the download path could not serve", func(t *testing.T) {
		wheel := buildWheel(t, "shapes", "1.0.0", ">=3.9")
		for _, filename := range []string{
			".shapes-1.0.0-py3-none-any.whl",
			"shapes-1.0.0.exe",
			"shapes-1.0.0-any.whl",
		} {
			res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
				"name": "shapes", "version": "1.0.0",
			}, filename, wheel)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("filename %q: status = %d, want 400; body %s", filename, res.StatusCode, res.Body)
			}
		}
		assertProjectAbsent(t, client, "shapes")
	})

	t.Run("rejects a name that normalizes to nothing", func(t *testing.T) {
		wheel := buildWheel(t, "empty-name", "1.0.0", ">=3.9")
		res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "___", "version": "1.0.0",
		}, "___-1.0.0-py3-none-any.whl", wheel)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", res.StatusCode, res.Body)
		}
	})

	t.Run("a repository that does not speak pypi is not found", func(t *testing.T) {
		wheel := buildWheel(t, "wrong-proto", "1.0.0", ">=3.9")
		res := multipartUpload(t, client, "/pypi/images/upload", map[string]string{
			"name": "wrong-proto", "version": "1.0.0",
		}, "wrong_proto-1.0.0-py3-none-any.whl", wheel)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("upload status = %d, want 404; body %s", res.StatusCode, res.Body)
		}
		index := mustDo(t, client, http.MethodGet, "/pypi/images/simple/", nil, "", "")
		if index.StatusCode != http.StatusNotFound {
			t.Fatalf("simple index status = %d, want 404", index.StatusCode)
		}
	})

	t.Run("publishing identical bytes twice is idempotent", func(t *testing.T) {
		wheel := buildWheel(t, "idem", "1.0.0", ">=3.9")
		fields := map[string]string{"name": "idem", "version": "1.0.0"}
		for i := range 2 {
			res := multipartUpload(t, client, "/pypi/internal/upload", fields,
				"idem-1.0.0-py3-none-any.whl", wheel)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("upload %d: status = %d; body %s", i+1, res.StatusCode, res.Body)
			}
		}
		if files := indexFiles(t, client, "idem"); len(files) != 1 {
			t.Fatalf("index lists %d files after two identical uploads, want 1", len(files))
		}
	})

	t.Run("refuses to replace published bytes", func(t *testing.T) {
		filename := "pinned-1.0.0-py3-none-any.whl"
		fields := map[string]string{"name": "pinned", "version": "1.0.0"}

		first := buildWheel(t, "pinned", "1.0.0", ">=3.9")
		if res := multipartUpload(t, client, "/pypi/internal/upload", fields, filename, first); res.StatusCode != http.StatusOK {
			t.Fatalf("first upload: %d %s", res.StatusCode, res.Body)
		}

		second := buildWheel(t, "pinned", "1.0.0", ">=3.11")
		if bytes.Equal(first, second) {
			t.Fatal("test wheels are identical, so this asserts nothing")
		}
		res := multipartUpload(t, client, "/pypi/internal/upload", fields, filename, second)
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("second upload: status = %d, want 409; body %s", res.StatusCode, res.Body)
		}
		// Same version, so this is the placement conflict and not the
		// cross-version guard the next case exercises.
		if strings.Contains(res.Body, "another version") {
			t.Fatalf("409 came from the wrong guard: %s", res.Body)
		}

		dl := mustDo(t, client, http.MethodGet, "/pypi/internal/packages/pinned/"+filename, nil, "", "")
		if dl.Body != string(first) {
			t.Fatal("download no longer returns the originally published bytes")
		}
		sum := sha256.Sum256(first)
		if files := indexFiles(t, client, "pinned"); len(files) != 1 ||
			files[0].Hashes["sha256"] != hex.EncodeToString(sum[:]) {
			t.Fatalf("index no longer advertises the original hash: %+v", files)
		}
	})

	// The files unique constraint is per version, so only a guard inside the
	// publish transaction keeps one filename from resolving to two blobs on
	// the version-less download path. Two escaping-equivalent spellings of one
	// PEP 440 epoch version are the way to reach it.
	t.Run("refuses one filename under two versions", func(t *testing.T) {
		filename := "epoch_demo-1_2.0-py3-none-any.whl"

		first := buildWheel(t, "epoch-demo", "1!2.0", ">=3.9")
		if res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "epoch-demo", "version": "1!2.0",
		}, filename, first); res.StatusCode != http.StatusOK {
			t.Fatalf("first upload: %d %s", res.StatusCode, res.Body)
		}

		second := buildWheel(t, "epoch-demo", "1_2.0", ">=3.9")
		res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "epoch-demo", "version": "1_2.0",
		}, filename, second)
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("second upload: status = %d, want 409; body %s", res.StatusCode, res.Body)
		}
		if !strings.Contains(res.Body, "another version") {
			t.Fatalf("409 did not come from the cross-version guard: %s", res.Body)
		}
		if files := indexFiles(t, client, "epoch-demo"); len(files) != 1 {
			t.Fatalf("index lists %d files for one filename, want 1", len(files))
		}
	})

	// The roadmap's M4 warning: accepting two spellings as two projects is
	// what would make the M5 name-claim rule unenforceable.
	t.Run("normalization is the uniqueness key", func(t *testing.T) {
		wheel := buildWheel(t, "Widget.Tool", "1.0.0", ">=3.9")
		if res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "Widget.Tool", "version": "1.0.0",
		}, "widget_tool-1.0.0-py3-none-any.whl", wheel); res.StatusCode != http.StatusOK {
			t.Fatalf("wheel upload: %d %s", res.StatusCode, res.Body)
		}

		sdist := buildSdist(t, "Widget.Tool", "2.0.0", ">=3.10")
		if res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
			"name": "widget_tool", "version": "2.0.0",
		}, "widget_tool-2.0.0.tar.gz", sdist); res.StatusCode != http.StatusOK {
			t.Fatalf("sdist upload: %d %s", res.StatusCode, res.Body)
		}

		files := indexFiles(t, client, "widget-tool")
		if len(files) != 2 {
			t.Fatalf("index lists %d files for two spellings of one project, want 2: %+v", len(files), files)
		}
		for _, spelling := range []string{"Widget.Tool", "widget_tool", "WIDGET--TOOL"} {
			page := mustDo(t, client, http.MethodGet, "/pypi/internal/simple/"+spelling+"/", nil, "", "")
			if page.StatusCode != http.StatusMovedPermanently {
				t.Fatalf("%q: status = %d, want a 301 onto the normalized page", spelling, page.StatusCode)
			}
			if loc := page.Header.Get("Location"); !strings.HasSuffix(loc, "/simple/widget-tool/") {
				t.Fatalf("%q redirects to %q", spelling, loc)
			}
		}

		// The sdist round-trips byte-identically off the normalized path.
		dl := mustDo(t, client, http.MethodGet,
			"/pypi/internal/packages/widget-tool/widget_tool-2.0.0.tar.gz", nil, "", "")
		if dl.StatusCode != http.StatusOK || dl.Body != string(sdist) {
			t.Fatalf("sdist download: %d, %d bytes of %d", dl.StatusCode, len(dl.Body), len(sdist))
		}
		if got := dl.Header.Get("Content-Type"); got != "application/gzip" {
			t.Fatalf("sdist Content-Type = %q", got)
		}
	})

	t.Run("a project name is one path segment", func(t *testing.T) {
		res := mustDo(t, client, http.MethodGet, "/pypi/internal/simple/idem/extra/", nil, "", "")
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body %s", res.StatusCode, res.Body)
		}
	})
}

type indexFile struct {
	Filename string            `json:"filename"`
	Hashes   map[string]string `json:"hashes"`
}

func indexFiles(t *testing.T, h http.Handler, project string) []indexFile {
	t.Helper()
	res := mustDo(t, h, http.MethodGet, "/pypi/internal/simple/"+project+"/", nil, "",
		"application/vnd.pypi.simple.v1+json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index for %s: %d %s", project, res.StatusCode, res.Body)
	}
	var body struct {
		Files []indexFile `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("decode index for %s: %v\n%s", project, err, res.Body)
	}
	return body.Files
}

// assertProjectAbsent fails if a rejected upload placed anything anyway.
func assertProjectAbsent(t *testing.T, h http.Handler, project string) {
	t.Helper()
	res := mustDo(t, h, http.MethodGet, "/pypi/internal/simple/"+project+"/", nil, "", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("project %s exists after a rejected upload: %d %s", project, res.StatusCode, res.Body)
	}
}

func buildSdist(t *testing.T, name, version, requiresPython string) []byte {
	t.Helper()
	meta := "Name: " + name + "\nVersion: " + version + "\nRequires-Python: " + requiresPython + "\n"
	root := strings.ReplaceAll(name, ".", "_") + "-" + version

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, f := range []struct{ name, body string }{
		{root + "/PKG-INFO", meta},
		{root + "/setup.py", "# " + version + "\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name:     f.name,
			Mode:     0o644,
			Size:     int64(len(f.body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("WriteHeader %s: %v", f.name, err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("Write %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

// TestPyPI_IndexShape covers what the index promises a client: canonical URLs,
// the declared API version, links that do not depend on the request, and a
// root listing that is not one page of a longer list.
func TestPyPI_IndexShape(t *testing.T) {
	client := newTestHandler(t)
	createRepo(t, client, "internal", "pypi")

	wheel := buildWheel(t, "shape-demo", "1.0.0", ">=3.9")
	if res := multipartUpload(t, client, "/pypi/internal/upload", map[string]string{
		"name": "shape-demo", "version": "1.0.0",
	}, "shape_demo-1.0.0-py3-none-any.whl", wheel); res.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d %s", res.StatusCode, res.Body)
	}

	t.Run("index urls are canonical", func(t *testing.T) {
		for path, want := range map[string]string{
			"/pypi/internal/simple":            "/pypi/internal/simple/",
			"/pypi/internal/simple/shape-demo": "/pypi/internal/simple/shape-demo/",
			"/pypi/internal/simple/Shape.Demo": "/pypi/internal/simple/shape-demo/",
		} {
			res := mustDo(t, client, http.MethodGet, path, nil, "", "")
			if res.StatusCode != http.StatusMovedPermanently {
				t.Fatalf("%s: status = %d, want 301", path, res.StatusCode)
			}
			if got := res.Header.Get("Location"); got != want {
				t.Fatalf("%s redirects to %q, want %q", path, got, want)
			}
		}
	})

	t.Run("links do not depend on the request", func(t *testing.T) {
		// A Host header is client input, so it must not reach a link that the
		// index hands back to that same client.
		res := mustDoWithHost(t, client, "/pypi/internal/simple/shape-demo/", "evil.example", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; body %s", res.StatusCode, res.Body)
		}
		if strings.Contains(res.Body, "evil.example") || strings.Contains(res.Body, "http://") {
			t.Fatalf("index reflected the request into a link: %s", res.Body)
		}
		want := "/pypi/internal/packages/shape-demo/shape_demo-1.0.0-py3-none-any.whl"
		if !strings.Contains(res.Body, want) {
			t.Fatalf("index does not carry the root-relative download path: %s", res.Body)
		}

		js := mustDoWithHost(t, client, "/pypi/internal/simple/shape-demo/", "evil.example", mediaSimpleJSONAccept)
		var body struct {
			Meta  map[string]string `json:"meta"`
			Files []struct {
				URL  string `json:"url"`
				Size int64  `json:"size"`
			} `json:"files"`
			Versions []string `json:"versions"`
		}
		if err := json.Unmarshal([]byte(js.Body), &body); err != nil {
			t.Fatalf("decode: %v\n%s", err, js.Body)
		}
		if len(body.Files) != 1 || body.Files[0].URL != want {
			t.Fatalf("JSON url = %+v, want %q", body.Files, want)
		}
		if body.Files[0].Size != int64(len(wheel)) {
			t.Fatalf("JSON size = %d, want %d", body.Files[0].Size, len(wheel))
		}
		if body.Versions == nil {
			t.Fatal("JSON index omits versions, which api-version 1.1 requires")
		}
		// PEP 700's versions and size are what makes this 1.1 rather than 1.0.
		if body.Meta["api-version"] != "1.1" {
			t.Fatalf("JSON api-version = %q, want 1.1", body.Meta["api-version"])
		}
	})

	t.Run("html declares its repository version", func(t *testing.T) {
		for _, path := range []string{"/pypi/internal/simple/", "/pypi/internal/simple/shape-demo/"} {
			res := mustDo(t, client, http.MethodGet, path, nil, "", "")
			if !strings.Contains(res.Body, `<meta name="pypi:repository-version" content="1.0">`) {
				t.Fatalf("%s has no PEP 629 meta tag: %s", path, res.Body)
			}
		}
	})

	// rootPageLimit is 200, and a page of a longer list is indistinguishable
	// from the whole list to a client.
	t.Run("root index lists every project", func(t *testing.T) {
		createRepo(t, client, "paged", "pypi")
		const projects = 205
		for i := range projects {
			name := fmt.Sprintf("bulk-%03d", i)
			publishFile(t, client, "paged", name, "1.0.0", name+"-1.0.0.tar.gz", []byte(name))
		}

		html := mustDo(t, client, http.MethodGet, "/pypi/paged/simple/", nil, "", "")
		if got := strings.Count(html.Body, `<a href="bulk-`); got != projects {
			t.Fatalf("root HTML lists %d projects, want %d", got, projects)
		}
		if !strings.Contains(html.Body, `<a href="bulk-204/">`) {
			t.Fatal("root HTML stops before the last project")
		}

		res := mustDo(t, client, http.MethodGet, "/pypi/paged/simple/", nil, "", mediaSimpleJSONAccept)
		var body struct {
			Projects []struct {
				Name string `json:"name"`
			} `json:"projects"`
		}
		if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Projects) != projects {
			t.Fatalf("root JSON lists %d projects, want %d", len(body.Projects), projects)
		}
	})
}

const mediaSimpleJSONAccept = "application/vnd.pypi.simple.v1+json"

// publishFile seeds a file over the management API, which is cheaper than
// building a distribution when only the placement rows matter.
func publishFile(t *testing.T, h http.Handler, repo, pkg, version, filename string, body []byte) {
	t.Helper()
	path := "/api/v1/repositories/" + repo + "/packages/" + pkg +
		"/versions/" + version + "/files/" + filename
	res := mustDo(t, h, http.MethodPut, path, body, "application/gzip", "")
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		t.Fatalf("publish %s: %d %s", filename, res.StatusCode, res.Body)
	}
}

func mustDoWithHost(t *testing.T, h http.Handler, path, host, accept string) httpResult {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Host = host
	req.Header.Set("X-Forwarded-Proto", "https")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return httpResult{StatusCode: rec.Code, Body: rec.Body.String(), Header: rec.Header()}
}
