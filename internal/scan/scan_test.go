package scan

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/storage/memstore"
)

// fakeAttacher records attachments, reading each body as the real service does.
type fakeAttacher struct {
	mu       sync.Mutex
	attached []attachment
}

type attachment struct {
	params evidence.AttachParams
	body   []byte
}

func (f *fakeAttacher) Attach(_ context.Context, p evidence.AttachParams) (evidence.Evidence, error) {
	body, err := io.ReadAll(p.Body)
	if err != nil {
		return evidence.Evidence{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = append(f.attached, attachment{params: p, body: body})
	return evidence.Evidence{ID: "ev-" + string(rune('a'+len(f.attached)-1)), Subject: p.Subject, Type: p.Type}, nil
}

type fixture struct {
	t     *testing.T
	store *cas.Store
	ev    *fakeAttacher
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, store: cas.New(memstore.New()), ev: &fakeAttacher{}}
}

func (f *fixture) put(payload []byte) model.Digest {
	f.t.Helper()
	info, err := f.store.PutBytes(f.t.Context(), payload)
	if err != nil {
		f.t.Fatalf("PutBytes: %v", err)
	}
	return info.Digest
}

func (f *fixture) scanner(opts Options) *Scanner {
	return New(f.store, f.ev, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// image stores a config, one layer and a manifest, returning the manifest digest.
func (f *fixture) image(mediaType string) model.Digest {
	f.t.Helper()
	cfgBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfg := f.put(cfgBytes)
	layerBytes := []byte("layer bytes for " + mediaType)
	layer := f.put(layerBytes)
	m := ocispec.Manifest{
		MediaType: mediaType,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(cfg.String()), Size: int64(len(cfgBytes))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: godigest.Digest(layer.String()), Size: int64(len(layerBytes))}},
	}
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.put(raw)
}

func (f *fixture) job(digest model.Digest, kind string) queue.Job {
	args, _ := json.Marshal(map[string]string{"digest": digest.String(), "kind": kind})
	return queue.Job{ID: 7, Kind: JobKind, Args: args}
}

func stubPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the trivy stub is a POSIX shell script")
	}
	p, err := filepath.Abs(filepath.Join("testdata", "trivy-stub.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tarGz(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := bodies[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStage_ManifestBecomesOCILayout(t *testing.T) {
	for _, mt := range []string{ocispec.MediaTypeImageManifest, mediaTypeDockerManifest} {
		t.Run(mt, func(t *testing.T) {
			f := newFixture(t)
			digest := f.image(mt)
			root := t.TempDir()

			targets, err := f.scanner(Options{}).stage(t.Context(), root, model.SubjectKindOCIManifest, digest)
			if err != nil {
				t.Fatalf("stage: %v", err)
			}
			if len(targets) != 1 || targets[0].coverage != "image" || targets[0].platform != "" {
				t.Fatalf("targets = %+v, want one image target", targets)
			}
			dir := targets[0].dir
			if got := string(readFile(t, filepath.Join(dir, "oci-layout"))); !strings.Contains(got, `"imageLayoutVersion":"1.0.0"`) {
				t.Errorf("oci-layout = %s", got)
			}
			var idx ocispec.Index
			if err := json.Unmarshal(readFile(t, filepath.Join(dir, "index.json")), &idx); err != nil {
				t.Fatalf("index.json: %v", err)
			}
			if len(idx.Manifests) != 1 || idx.Manifests[0].Digest.String() != digest.String() || idx.Manifests[0].MediaType != mt {
				t.Errorf("index.json manifests = %+v", idx.Manifests)
			}
			manifestBlob := readFile(t, filepath.Join(dir, "blobs", "sha256", digest.Hex()))
			if int64(len(manifestBlob)) != idx.Manifests[0].Size {
				t.Errorf("manifest blob is %d bytes, descriptor says %d", len(manifestBlob), idx.Manifests[0].Size)
			}
			entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
			if err != nil || len(entries) != 3 {
				t.Errorf("blobs = %d entries (err %v), want manifest, config, layer", len(entries), err)
			}
		})
	}
}

func TestStage_ManifestWithMissingLayerIsAnError(t *testing.T) {
	f := newFixture(t)
	cfg := f.put([]byte(`{}`))
	missing := model.NewDigest(model.AlgorithmSHA256, bytes.Repeat([]byte{1}, 32))
	m := ocispec.Manifest{
		Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(cfg.String()), Size: 2},
		Layers: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: godigest.Digest(missing.String()), Size: 5}},
	}
	raw, _ := json.Marshal(m)
	digest := f.put(raw)
	_, err := f.scanner(Options{}).stage(t.Context(), t.TempDir(), model.SubjectKindOCIManifest, digest)
	if err == nil {
		t.Fatal("stage succeeded with a layer missing from the CAS")
	}
}

func TestStage_IndexStagesEachPlatformAndSkipsAttestations(t *testing.T) {
	f := newFixture(t)
	amd := f.image(ocispec.MediaTypeImageManifest)
	arm := f.image(mediaTypeDockerManifest)
	idx := ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(amd.String()), Size: 1, Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(amd.String()), Size: 1, Annotations: map[string]string{annotationReferenceType: attestationManifest}, Platform: &ocispec.Platform{OS: "unknown", Architecture: "unknown"}},
		{MediaType: ocispec.MediaTypeImageIndex, Digest: godigest.Digest(amd.String()), Size: 1},
		{MediaType: mediaTypeDockerManifest, Digest: godigest.Digest(arm.String()), Size: 1, Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
	}}
	raw, _ := json.Marshal(idx)
	digest := f.put(raw)

	targets, err := f.scanner(Options{}).stage(t.Context(), t.TempDir(), model.SubjectKindOCIIndex, digest)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	var platforms []string
	for _, tg := range targets {
		platforms = append(platforms, tg.platform)
	}
	if got := strings.Join(platforms, ","); got != "linux/amd64,linux/arm64/v8" {
		t.Fatalf("platforms = %s, want linux/amd64,linux/arm64/v8", got)
	}
}

func TestStage_WheelIsUnpackedWithinLimits(t *testing.T) {
	f := newFixture(t)
	digest := f.put(zipOf(t, map[string]string{
		"pkg/__init__.py":            "x = 1\n",
		"pkg-1.0.dist-info/METADATA": "Name: pkg\nVersion: 1.0\n",
	}))
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), t.TempDir(), model.SubjectKindPyPIDist, digest)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if targets[0].coverage != "package" {
		t.Errorf("coverage = %s", targets[0].coverage)
	}
	if got := string(readFile(t, filepath.Join(targets[0].dir, "pkg-1.0.dist-info", "METADATA"))); !strings.Contains(got, "Version: 1.0") {
		t.Errorf("METADATA = %q", got)
	}
	info, err := os.Stat(filepath.Join(targets[0].dir, "pkg", "__init__.py"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
}

func TestStage_SdistAndNPMTarballAreDetectedByContent(t *testing.T) {
	f := newFixture(t)
	digest := f.put(tarGz(t,
		[]tar.Header{{Name: "package/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "package/package.json", Typeflag: tar.TypeReg, Mode: 0o644}},
		map[string]string{"package/package.json": `{"name":"x","version":"1.0.0"}`}))
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), t.TempDir(), model.SubjectKindNPMTarball, digest)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if got := readFile(t, filepath.Join(targets[0].dir, "package", "package.json")); !strings.Contains(string(got), `"version"`) {
		t.Errorf("package.json = %q", got)
	}
}

func TestStage_ArchiveOverLimitIsRefused(t *testing.T) {
	big := strings.Repeat("a", 4096)
	cases := map[string][]byte{
		"zip": zipOf(t, map[string]string{"big.txt": big}),
		"tgz": tarGz(t, []tar.Header{{Name: "big.txt", Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{"big.txt": big}),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			digest := f.put(archive)
			_, err := f.scanner(Options{MaxUnpackedBytes: 1000}).stage(t.Context(), t.TempDir(), model.SubjectKindNPMTarball, digest)
			if !errors.Is(err, ErrArchiveTooLarge) {
				t.Fatalf("stage error = %v, want ErrArchiveTooLarge", err)
			}
		})
	}
}

func TestStage_TarballPathTraversalIsRefused(t *testing.T) {
	for _, name := range []string{"../x", "/abs", "a/../../x"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			digest := f.put(tarGz(t, []tar.Header{{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{name: "x"}))
			root := t.TempDir()
			_, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), root, model.SubjectKindNPMTarball, digest)
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("stage error = %v, want ErrUnsafePath", err)
			}
		})
	}
}

func TestStage_SymlinksAreSkipped(t *testing.T) {
	f := newFixture(t)
	digest := f.put(tarGz(t,
		[]tar.Header{
			{Name: "package/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
			{Name: "package/hard", Typeflag: tar.TypeLink, Linkname: "package/real"},
			{Name: "package/real", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		map[string]string{"package/real": "ok"}))
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), t.TempDir(), model.SubjectKindNPMTarball, digest)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	for _, name := range []string{"link", "hard"} {
		if _, err := os.Lstat(filepath.Join(targets[0].dir, "package", name)); !os.IsNotExist(err) {
			t.Errorf("%s was created (err %v)", name, err)
		}
	}
	if string(readFile(t, filepath.Join(targets[0].dir, "package", "real"))) != "ok" {
		t.Error("regular file missing")
	}
}

func TestStage_FileKindIsUnsupported(t *testing.T) {
	f := newFixture(t)
	_, err := f.scanner(Options{}).stage(t.Context(), t.TempDir(), model.SubjectKindFile, f.put([]byte("x")))
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("stage error = %v, want ErrUnsupportedKind", err)
	}
}

func TestReport_CountsSeverities(t *testing.T) {
	cases := []struct {
		name   string
		report string
		want   map[string]int
	}{
		{"no results", `{"SchemaVersion":2}`, map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0, "UNKNOWN": 0}},
		{"mixed with unrecognised severity",
			`{"Results":[{"Vulnerabilities":[{"Severity":"CRITICAL"},{"Severity":"HIGH"},{"Severity":"HIGH"}]},{"Vulnerabilities":[{"Severity":"LOW"},{"Severity":"BOGUS"},{"Severity":"UNKNOWN"}]},{"Target":"no vulns"}]}`,
			map[string]int{"CRITICAL": 1, "HIGH": 2, "MEDIUM": 0, "LOW": 1, "UNKNOWN": 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := countSeverities(strings.NewReader(tc.report))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("counts = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %d, want %d", k, got[k], v)
				}
			}
		})
	}
	if _, err := countSeverities(strings.NewReader("not json")); err == nil {
		t.Error("countSeverities accepted invalid JSON")
	}
}

func TestRun_FileKindIsSkipped(t *testing.T) {
	f := newFixture(t)
	res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), f.job(f.put([]byte("x")), "file"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(res) != `{"skipped":"unsupported kind"}` || len(f.ev.attached) != 0 {
		t.Fatalf("result = %s, attached = %d", res, len(f.ev.attached))
	}
}

func TestRun_RejectsMalformedArgs(t *testing.T) {
	f := newFixture(t)
	s := f.scanner(Options{})
	for name, args := range map[string]string{
		"bad json":   `{`,
		"bad digest": `{"digest":"sha256:zz","kind":"oci-manifest"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Handle(t.Context(), queue.Job{ID: 1, Args: json.RawMessage(args)}); err == nil {
				t.Fatal("Handle accepted bad args")
			}
		})
	}
}

func TestRun_ImageAttachesSbomAndScanWithProducerMetadata(t *testing.T) {
	f := newFixture(t)
	digest := f.image(ocispec.MediaTypeImageManifest)
	tmp := t.TempDir()
	res, err := f.scanner(Options{Trivy: stubPath(t), TempDir: tmp}).Handle(t.Context(), f.job(digest, "oci-manifest"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.ev.attached) != 2 {
		t.Fatalf("attached %d, want sbom and scan", len(f.ev.attached))
	}
	sbom, scan := f.ev.attached[0], f.ev.attached[1]
	if sbom.params.Type != model.EvidenceSBOM || sbom.params.MediaType != MediaTypeSBOM ||
		scan.params.Type != model.EvidenceScan || scan.params.MediaType != MediaTypeScan {
		t.Errorf("types = %s/%s, media = %s/%s", sbom.params.Type, scan.params.Type, sbom.params.MediaType, scan.params.MediaType)
	}
	for _, a := range f.ev.attached {
		if a.params.Actor != Actor || a.params.Subject != digest {
			t.Errorf("actor/subject = %s/%s", a.params.Actor, a.params.Subject)
		}
		var meta map[string]any
		if err := json.Unmarshal(a.params.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if meta["producer"] != "sindri" || meta["scanner"] != "trivy" || meta["coverage"] != "image" ||
			meta["scanner_version"] != "0.58.1" || meta["db_updated_at"] != "2026-11-10T12:00:00.000000000Z" {
			t.Errorf("metadata = %v", meta)
		}
		if _, ok := meta["platform"]; ok {
			t.Errorf("platform set for a plain manifest: %v", meta)
		}
		counts, _ := meta["severity_counts"].(map[string]any)
		if counts["CRITICAL"] != float64(1) || counts["HIGH"] != float64(1) || counts["LOW"] != float64(0) {
			t.Errorf("severity_counts = %v", counts)
		}
	}
	if !bytes.Contains(sbom.body, []byte("CycloneDX")) || !bytes.Contains(scan.body, []byte("CVE-2026-0001")) {
		t.Error("evidence bodies are not the sbom and report")
	}
	if !strings.Contains(string(res), `"sbom_id"`) {
		t.Errorf("result = %s", res)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("staging directories left behind: %d", len(entries))
	}
}

func TestRun_PackageUsesRootfsAndPackageCoverage(t *testing.T) {
	f := newFixture(t)
	digest := f.put(zipOf(t, map[string]string{"a.txt": "x"}))
	if _, err := f.scanner(Options{Trivy: stubPath(t), MaxUnpackedBytes: 1 << 20}).Handle(t.Context(), f.job(digest, "pypi-dist")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var meta map[string]any
	_ = json.Unmarshal(f.ev.attached[1].params.Metadata, &meta)
	if meta["coverage"] != "package" || meta["severity_counts"].(map[string]any)["MEDIUM"] != float64(1) {
		t.Errorf("metadata = %v", meta)
	}
}

func TestRun_IndexAttachesOnePairPerPlatform(t *testing.T) {
	f := newFixture(t)
	amd := f.image(ocispec.MediaTypeImageManifest)
	arm := f.image(mediaTypeDockerManifest)
	raw, _ := json.Marshal(ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(amd.String()), Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: mediaTypeDockerManifest, Digest: godigest.Digest(arm.String()), Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
	}})
	digest := f.put(raw)
	if _, err := f.scanner(Options{Trivy: stubPath(t)}).Handle(t.Context(), f.job(digest, "oci-index")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.ev.attached) != 4 {
		t.Fatalf("attached %d, want 4", len(f.ev.attached))
	}
	var got []string
	for _, a := range f.ev.attached {
		var meta evidenceMetadata
		_ = json.Unmarshal(a.params.Metadata, &meta)
		got = append(got, string(a.params.Type)+":"+meta.Platform)
		if a.params.Subject != digest {
			t.Errorf("subject = %s, want the index", a.params.Subject)
		}
	}
	want := "sbom:linux/amd64,scan:linux/amd64,sbom:linux/arm64,scan:linux/arm64"
	if strings.Join(got, ",") != want {
		t.Errorf("attachments = %v, want %s", got, want)
	}
}

func TestRun_TrivyFailureReturnsErrorAndAttachesNothing(t *testing.T) {
	t.Setenv("TRIVY_STUB_FAIL", "1")
	f := newFixture(t)
	digest := f.image(ocispec.MediaTypeImageManifest)
	_, err := f.scanner(Options{Trivy: stubPath(t)}).Handle(t.Context(), f.job(digest, "oci-manifest"))
	if err == nil || !strings.Contains(err.Error(), "vulnerability database unavailable") {
		t.Fatalf("Handle error = %v, want stderr in the message", err)
	}
	if len(f.ev.attached) != 0 {
		t.Fatalf("attached %d, want none", len(f.ev.attached))
	}
}

func TestRun_PassesMirrorAndSkipUpdateFlags(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("TRIVY_STUB_ARGS", argsFile)
	f := newFixture(t)
	digest := f.image(ocispec.MediaTypeImageManifest)
	s := f.scanner(Options{Trivy: stubPath(t), DBRepository: "registry.example/trivy-db", CacheDir: "/cache", SkipDBUpdate: true})
	if _, err := s.Handle(t.Context(), f.job(digest, "oci-manifest")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(readFile(t, argsFile))), "\n")
	var image, convert string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "image "):
			image = l
		case strings.HasPrefix(l, "convert "):
			convert = l
		}
	}
	for _, want := range []string{"--input ", "--format json", "--list-all-pkgs", "--scanners vuln", "--quiet",
		"--cache-dir /cache", "--db-repository registry.example/trivy-db", "--skip-db-update", "--output "} {
		if !strings.Contains(image, want) {
			t.Errorf("image args %q lack %q", image, want)
		}
	}
	if !strings.Contains(convert, "--format cyclonedx") {
		t.Errorf("convert args = %q", convert)
	}
}
