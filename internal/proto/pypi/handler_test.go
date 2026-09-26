package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func TestNormalizeName_PEP503(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "already normalized", in: "demo-package", want: "demo-package"},
		{name: "dots and underscores", in: "Foo.Bar_Baz", want: "foo-bar-baz"},
		{name: "mixed separators", in: "foo__bar..baz", want: "foo-bar-baz"},
		{name: "upper case", in: "Demo.Package", want: "demo-package"},
		{name: "trim spaces", in: "  Foo.Bar  ", want: "foo-bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeName(tc.in); got != tc.want {
				t.Fatalf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitPyPIPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		wantRepo string
		wantRest string
		wantKind pathKind
		wantOK   bool
	}{
		{name: "simple root", path: "internal/simple/", wantRepo: "internal", wantRest: "", wantKind: pathSimple, wantOK: true},
		{name: "simple root no slash", path: "internal/simple", wantRepo: "internal", wantRest: "", wantKind: pathSimple, wantOK: true},
		{name: "simple project", path: "internal/simple/demo-package/", wantRepo: "internal", wantRest: "demo-package", wantKind: pathSimple, wantOK: true},
		{name: "upload", path: "internal/upload", wantRepo: "internal", wantRest: "", wantKind: pathUpload, wantOK: true},
		{name: "packages", path: "internal/packages/demo-package/file.whl", wantRepo: "internal", wantRest: "demo-package/file.whl", wantKind: pathPackages, wantOK: true},
		{name: "slash repo simple", path: "team/internal/simple/foo", wantRepo: "team/internal", wantRest: "foo", wantKind: pathSimple, wantOK: true},
		{name: "project named packages", path: "internal/simple/packages/", wantRepo: "internal", wantRest: "packages", wantKind: pathSimple, wantOK: true},
		{name: "leftmost marker wins", path: "internal/simple/upload/", wantRepo: "internal", wantRest: "upload", wantKind: pathSimple, wantOK: true},
		{name: "nested repo download", path: "team/internal/packages/demo/f.whl", wantRepo: "team/internal", wantRest: "demo/f.whl", wantKind: pathPackages, wantOK: true},
		{name: "marker cannot be the first segment", path: "simple/foo", wantOK: false},
		{name: "invalid", path: "internal/other", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, rest, kind, ok := splitPyPIPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if repo != tc.wantRepo || rest != tc.wantRest || kind != tc.wantKind {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)",
					repo, rest, kind, tc.wantRepo, tc.wantRest, tc.wantKind)
			}
		})
	}
}

func TestPrefersJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		accept string
		want   bool
	}{
		{name: "empty defaults html", accept: "", want: false},
		{name: "pypi json", accept: mediaSimpleJSON, want: true},
		{name: "json before html", accept: mediaSimpleJSON + ", " + mediaSimpleHTML, want: true},
		{name: "html before json", accept: "text/html, " + mediaSimpleJSON, want: false},
		{name: "browser", accept: "text/html,application/xhtml+xml", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := prefersJSON(tc.accept); got != tc.want {
				t.Fatalf("prefersJSON(%q) = %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}

func TestParseCoreMetadata(t *testing.T) {
	t.Parallel()
	raw := []byte("Name: Demo.Package\nVersion: 1.0.0\nRequires-Python: >=3.9\nSummary: hello\n\nBody\n")
	m := parseCoreMetadata(raw)
	if m.Name != "Demo.Package" || m.Version != "1.0.0" || m.RequiresPython != ">=3.9" {
		t.Fatalf("parseCoreMetadata = %+v", m)
	}
}

func TestExtractMetadata_Wheel(t *testing.T) {
	t.Parallel()
	body := buildTestWheel(t, "demo_package-1.0.0.dist-info/METADATA",
		"Name: demo-package\nVersion: 1.0.0\nRequires-Python: >=3.10\n")
	m, err := extractMetadata("demo_package-1.0.0-py3-none-any.whl", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("extractMetadata: %v", err)
	}
	if m.RequiresPython != ">=3.10" || m.Name != "demo-package" {
		t.Fatalf("metadata = %+v", m)
	}
}

func TestExtractMetadata_Sdist(t *testing.T) {
	t.Parallel()
	body := buildTestSdist(t, "demo-package-1.0.0/PKG-INFO",
		"Name: demo-package\nVersion: 1.0.0\nRequires-Python: >=3.8\n")
	m, err := extractMetadata("demo-package-1.0.0.tar.gz", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("extractMetadata: %v", err)
	}
	if m.RequiresPython != ">=3.8" {
		t.Fatalf("RequiresPython = %q", m.RequiresPython)
	}
}

func buildTestWheel(t *testing.T, metaPath, metaBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(metaPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte(metaBody)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func buildTestSdist(t *testing.T, infoPath, infoBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name: infoPath,
		Mode: 0o644,
		Size: int64(len(infoBody)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte(infoBody)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestMediaTypeForFilename(t *testing.T) {
	t.Parallel()
	if got := mediaTypeForFilename("x.whl"); got != "application/zip" {
		t.Fatalf("whl = %q", got)
	}
	if got := mediaTypeForFilename("x.tar.gz"); got != "application/gzip" {
		t.Fatalf("sdist = %q", got)
	}
	if !strings.Contains(mediaTypeForFilename("x.egg"), "octet-stream") {
		t.Fatalf("other = %q", mediaTypeForFilename("x.egg"))
	}
}

func TestPrefersJSON_QualityValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		accept string
		want   bool
	}{
		{name: "pip", accept: mediaSimpleJSON + ", " + mediaSimpleHTML + ";q=0.2, text/html;q=0.01", want: true},
		{name: "quality beats position", accept: "text/html;q=0.1, application/json;q=0.9", want: true},
		{name: "html wins on quality", accept: mediaSimpleJSON + ";q=0.1, text/html;q=0.9", want: false},
		{name: "equal quality falls back to position", accept: mediaSimpleJSON + ";q=0.5, text/html;q=0.5", want: true},
		{name: "json refused outright", accept: mediaSimpleJSON + ";q=0, text/html", want: false},
		{name: "html refused outright", accept: "text/html;q=0, " + mediaSimpleJSON, want: true},
		{name: "wildcard only gets html", accept: "*/*", want: false},
		{name: "browser", accept: "text/html,application/xhtml+xml,*/*;q=0.8", want: false},
		{name: "charset parameter is not a quality", accept: mediaSimpleJSON + ";charset=utf-8", want: true},
		{name: "malformed quality is treated as one", accept: mediaSimpleJSON + ";q=notanumber, text/html;q=0.9", want: true},
		{name: "empty", accept: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := prefersJSON(tc.accept); got != tc.want {
				t.Fatalf("prefersJSON(%q) = %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}

func TestPackageURL_IsRootRelative(t *testing.T) {
	t.Parallel()
	// Nothing from the request may reach a link: a Host header is client input.
	got := packageURL("team/internal", "demo-package", "demo_package-1.0.0-py3-none-any.whl")
	want := "/pypi/team/internal/packages/demo-package/demo_package-1.0.0-py3-none-any.whl"
	if got != want {
		t.Fatalf("packageURL = %q, want %q", got, want)
	}
}
