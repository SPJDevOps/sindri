package scan

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
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
	t      *testing.T
	store  *cas.Store
	ev     *fakeAttacher
	placed *fakePlacement
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, store: cas.New(memstore.New()), ev: &fakeAttacher{}, placed: &fakePlacement{}}
}

const testRepo = "repo"

// fakePlacement lets a repository read everything until a test denies digests.
type fakePlacement struct {
	mu     sync.Mutex
	denied map[string]map[string]bool
}

// deny makes repo hold everything except the listed digests.
func (p *fakePlacement) deny(repo string, digests ...model.Digest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.denied == nil {
		p.denied = make(map[string]map[string]bool)
	}
	m := make(map[string]bool)
	for _, d := range digests {
		m[d.String()] = true
	}
	p.denied[repo] = m
}

func (p *fakePlacement) CheckPlacement(_ context.Context, arg db.CheckPlacementParams) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.denied[arg.RepoName][arg.Digest] {
		return false, nil
	}
	return true, nil
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
	return New(f.store, f.ev, f.placed, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// scannerEnv is scanner with extra environment for the Trivy stub, which the
// filtered environment would otherwise not carry.
func (f *fixture) scannerEnv(opts Options, env ...string) *Scanner {
	s := f.scanner(opts)
	s.extraEnv = env
	return s
}

// image stores a config, one layer and a manifest, returning the manifest digest.
func (f *fixture) image(mediaType string) model.Digest { return f.imageN(mediaType, 0) }

// imageN is image with a layer that differs per n, so digests differ.
func (f *fixture) imageN(mediaType string, n int) model.Digest {
	f.t.Helper()
	cfgBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfg := f.put(cfgBytes)
	layerBytes := []byte("layer bytes for " + mediaType + strconv.Itoa(n))
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
	args, _ := json.Marshal(map[string]string{"digest": digest.String(), "kind": kind, "repository": testRepo})
	return queue.Job{ID: 7, Kind: scanjob.JobKind, Args: args}
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

			targets, err := f.scanner(Options{}).stage(t.Context(), testRepo, root, model.SubjectKindOCIManifest, digest)
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
	_, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIManifest, digest)
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
		{MediaType: mediaTypeDockerManifest, Digest: godigest.Digest(arm.String()), Size: 1, Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
	}}
	raw, _ := json.Marshal(idx)
	digest := f.put(raw)

	targets, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIIndex, digest)
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
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindPyPIDist, digest)
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
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindNPMTarball, digest)
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
			_, err := f.scanner(Options{MaxUnpackedBytes: 1000}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindNPMTarball, digest)
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
			_, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), testRepo, root, model.SubjectKindNPMTarball, digest)
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
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindNPMTarball, digest)
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
	_, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindFile, f.put([]byte("x")))
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("stage error = %v, want ErrUnsupportedKind", err)
	}
}

func TestReport_ListsVulnerabilitiesDeduplicatedAndSorted(t *testing.T) {
	cases := []struct {
		name   string
		report string
		want   []vulnerability
	}{
		{"none gives an empty list, not nil", `{"SchemaVersion":2}`, []vulnerability{}},
		{"worst first, then by id, repeats collapsed",
			`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[
				{"VulnerabilityID":"CVE-2","Severity":"LOW"},
				{"VulnerabilityID":"CVE-9","Severity":"HIGH"},
				{"VulnerabilityID":"CVE-1","Severity":"HIGH"},
				{"VulnerabilityID":"CVE-5","Severity":"CRITICAL"}]},
				{"Vulnerabilities":[{"VulnerabilityID":"CVE-1","Severity":"HIGH"},{"VulnerabilityID":"CVE-3","Severity":"weird"}]}]}`,
			[]vulnerability{{"CVE-5", "CRITICAL"}, {"CVE-1", "HIGH"}, {"CVE-9", "HIGH"}, {"CVE-2", "LOW"}, {"CVE-3", "UNKNOWN"}}},
		{"same id at two severities is kept twice",
			`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-1","Severity":"LOW"},{"VulnerabilityID":"CVE-1","Severity":"HIGH"}]}]}`,
			[]vulnerability{{"CVE-1", "HIGH"}, {"CVE-1", "LOW"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := countSeverities(strings.NewReader(tc.report))
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("vulnerabilities = %v, want %v", got, tc.want)
			}
		})
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
			`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{"Severity":"CRITICAL"},{"Severity":"HIGH"},{"Severity":"HIGH"}]},{"Vulnerabilities":[{"Severity":"LOW"},{"Severity":"BOGUS"},{"Severity":"UNKNOWN"}]},{"Target":"no vulns"}]}`,
			map[string]int{"CRITICAL": 1, "HIGH": 2, "MEDIUM": 0, "LOW": 1, "UNKNOWN": 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := countSeverities(strings.NewReader(tc.report))
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
	if _, _, err := countSeverities(strings.NewReader("not json")); err == nil {
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
		if a.params.Actor != scanjob.Actor || a.params.Subject != digest {
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
		if a.params.Type == model.EvidenceSBOM {
			if _, ok := meta["vulnerabilities"]; ok {
				t.Errorf("sbom metadata carries vulnerabilities: %v", meta)
			}
			continue
		}
		vulns, ok := meta["vulnerabilities"].([]any)
		if !ok || len(vulns) != 2 {
			t.Fatalf("vulnerabilities = %v, want the two findings of the fixture", meta["vulnerabilities"])
		}
		if first, _ := vulns[0].(map[string]any); first["id"] != "CVE-2026-0001" || first["severity"] != "CRITICAL" {
			t.Errorf("vulnerabilities[0] = %v, want the CRITICAL finding first", vulns[0])
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
	if _, ok := meta["platforms"]; ok {
		t.Errorf("platforms set for a single-manifest subject: %v", meta)
	}
	if vulns, _ := meta["vulnerabilities"].([]any); len(vulns) != 1 {
		t.Errorf("vulnerabilities = %v, want the one finding of the fixture", meta["vulnerabilities"])
	}
}

func TestRun_IndexMetadataCarriesFullPlatformListOnScansOnly(t *testing.T) {
	f := newFixture(t)
	amd := f.image(ocispec.MediaTypeImageManifest)
	arm := f.image(mediaTypeDockerManifest)
	raw, _ := json.Marshal(ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: mediaTypeDockerManifest, Digest: godigest.Digest(arm.String()), Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(amd.String()), Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}},
	}})
	digest := f.put(raw)
	if _, err := f.scanner(Options{Trivy: stubPath(t)}).Handle(t.Context(), f.job(digest, "oci-index")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.ev.attached) != 4 {
		t.Fatalf("attached %d documents, want 4", len(f.ev.attached))
	}
	for _, a := range f.ev.attached {
		var meta map[string]any
		if err := json.Unmarshal(a.params.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(meta["platforms"]); got != "[linux/amd64 linux/arm64]" {
			t.Errorf("%s platforms = %v, want the sorted full list", a.params.Type, meta["platforms"])
		}
		_, hasVulns := meta["vulnerabilities"]
		if hasVulns != (a.params.Type == model.EvidenceScan) {
			t.Errorf("%s has vulnerabilities = %v, want only scan rows to carry it", a.params.Type, hasVulns)
		}
	}
}

func TestRun_IndexPlatformListIncludesPlatformlessChild(t *testing.T) {
	f := newFixture(t)
	arm := f.image(mediaTypeDockerManifest)
	bare := f.imageN(ocispec.MediaTypeImageManifest, 1)
	raw, _ := json.Marshal(ocispec.Index{Manifests: []ocispec.Descriptor{
		{MediaType: mediaTypeDockerManifest, Digest: godigest.Digest(arm.String()), Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
		{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(bare.String())},
	}})
	digest := f.put(raw)
	if _, err := f.scanner(Options{Trivy: stubPath(t)}).Handle(t.Context(), f.job(digest, "oci-index")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.ev.attached) != 4 {
		t.Fatalf("attached %d documents, want 4", len(f.ev.attached))
	}
	for _, a := range f.ev.attached {
		var meta struct {
			Platforms []string `json:"platforms"`
		}
		if err := json.Unmarshal(a.params.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(meta.Platforms, ","); got != ",linux/arm64" {
			t.Errorf("%s platforms = %q, want the platform-less key and linux/arm64", a.params.Type, got)
		}
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
	f := newFixture(t)
	digest := f.image(ocispec.MediaTypeImageManifest)
	_, err := f.scannerEnv(Options{Trivy: stubPath(t)}, "TRIVY_STUB_FAIL=1").Handle(t.Context(), f.job(digest, "oci-manifest"))
	if err == nil || !strings.Contains(err.Error(), "vulnerability database unavailable") {
		t.Fatalf("Handle error = %v, want stderr in the message", err)
	}
	if len(f.ev.attached) != 0 {
		t.Fatalf("attached %d, want none", len(f.ev.attached))
	}
}

func TestRun_PassesMirrorAndSkipUpdateFlags(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	f := newFixture(t)
	digest := f.image(ocispec.MediaTypeImageManifest)
	s := f.scannerEnv(Options{Trivy: stubPath(t), DBRepository: "registry.example/trivy-db", JavaDBRepository: "registry.example/java-db", CacheDir: "/cache", SkipDBUpdate: true},
		"TRIVY_STUB_ARGS="+argsFile)
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
		"--cache-dir /cache", "--db-repository registry.example/trivy-db", "--java-db-repository registry.example/java-db", "--skip-db-update", "--skip-java-db-update", "--output "} {
		if !strings.Contains(image, want) {
			t.Errorf("image args %q lack %q", image, want)
		}
	}
	if !strings.Contains(string(readFile(t, argsFile)), "--version --format json --cache-dir /cache") {
		t.Errorf("version probe lacks --cache-dir: %s", readFile(t, argsFile))
	}
	if !strings.Contains(convert, "--format cyclonedx") {
		t.Errorf("convert args = %q", convert)
	}
}

func TestReport_NonReportsAreErrorsNotZeroCounts(t *testing.T) {
	for name, doc := range map[string]string{
		"empty object":    `{}`,
		"null":            `null`,
		"array":           `[]`,
		"old schema":      `{"SchemaVersion":1}`,
		"trailing junk":   `{"SchemaVersion":2} garbage`,
		"second document": `{"SchemaVersion":2}{"SchemaVersion":2}`,
		"empty input":     ``,
	} {
		t.Run(name, func(t *testing.T) {
			if got, _, err := countSeverities(strings.NewReader(doc)); err == nil {
				t.Fatalf("countSeverities = %v, want an error", got)
			}
		})
	}
	if _, _, err := countSeverities(strings.NewReader(`{"SchemaVersion":2}`)); err != nil {
		t.Errorf("a schema 2 report without Results was refused: %v", err)
	}
}

func TestStage_ArchiveWithTooManyEntriesIsRefused(t *testing.T) {
	var hdrs []tar.Header
	for i := 0; i < 5; i++ {
		hdrs = append(hdrs, tar.Header{Name: "d" + strconv.Itoa(i), Typeflag: tar.TypeDir, Mode: 0o755})
	}
	hdrs = append(hdrs, tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "x"})
	f := newFixture(t)
	digest := f.put(tarGz(t, hdrs, nil))
	s := f.scanner(Options{MaxUnpackedBytes: 1 << 30})
	s.maxEntries = 4
	_, err := s.stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindNPMTarball, digest)
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("stage error = %v, want ErrArchiveTooLarge", err)
	}
}

func TestStage_EmptyFilesAreChargedPerEntry(t *testing.T) {
	var hdrs []tar.Header
	for i := 0; i < 10; i++ {
		hdrs = append(hdrs, tar.Header{Name: "f" + strconv.Itoa(i), Typeflag: tar.TypeReg, Mode: 0o644})
	}
	f := newFixture(t)
	digest := f.put(tarGz(t, hdrs, nil))
	_, err := f.scanner(Options{MaxUnpackedBytes: 1000}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindNPMTarball, digest)
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("stage error = %v, want ErrArchiveTooLarge", err)
	}
}

func TestStage_CancelledContextStopsUnpacking(t *testing.T) {
	f := newFixture(t)
	digest := f.put(zipOf(t, map[string]string{"a": "x"}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(ctx, testRepo, t.TempDir(), model.SubjectKindPyPIDist, digest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stage error = %v, want context.Canceled", err)
	}
}

func TestStage_ZipTraversalIsRefused(t *testing.T) {
	for _, name := range []string{"../x", "/abs"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			digest := f.put(zipOf(t, map[string]string{name: "x"}))
			_, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindPyPIDist, digest)
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("stage error = %v, want ErrUnsafePath", err)
			}
		})
	}
}

func TestStage_ZipSymlinksAreSkipped(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "link"}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("/etc/passwd"))
	w, _ = zw.Create("real")
	_, _ = w.Write([]byte("ok"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	targets, err := f.scanner(Options{MaxUnpackedBytes: 1 << 20}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindPyPIDist, f.put(buf.Bytes()))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(targets[0].dir, "link")); !os.IsNotExist(err) {
		t.Errorf("symlink was created (err %v)", err)
	}
	if string(readFile(t, filepath.Join(targets[0].dir, "real"))) != "ok" {
		t.Error("regular file missing")
	}
}

// A zip header can claim a small size for data that inflates to far more; the
// cap counts the bytes actually written.
func TestStage_ZipUnderstatedSizeIsStillCapped(t *testing.T) {
	data := []byte(strings.Repeat("a", 1<<16))
	var comp bytes.Buffer
	fw, _ := flate.NewWriter(&comp, flate.BestSpeed)
	_, _ = fw.Write(data)
	_ = fw.Close()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "big", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(data),
		CompressedSize64: uint64(comp.Len()), UncompressedSize64: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(comp.Bytes())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	_, err = f.scanner(Options{MaxUnpackedBytes: 2000}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindPyPIDist, f.put(buf.Bytes()))
	if err == nil {
		t.Fatal("stage accepted an archive that unpacks past the cap")
	}
	t.Logf("error: %v", err)
}

func (f *fixture) index(children ...ocispec.Descriptor) model.Digest {
	f.t.Helper()
	raw, err := json.Marshal(ocispec.Index{Manifests: children})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.put(raw)
}

func TestStage_IndexListingOneChildRepeatedlyStagesItOnce(t *testing.T) {
	f := newFixture(t)
	child := f.image(ocispec.MediaTypeImageManifest)
	var descs []ocispec.Descriptor
	for i := 0; i < 100; i++ {
		descs = append(descs, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(child.String()),
			Platform: &ocispec.Platform{OS: "linux", Architecture: "amd" + strconv.Itoa(i)},
		})
	}
	targets, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIIndex, f.index(descs...))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(targets) != 1 || targets[0].platform != "linux/amd0" {
		t.Fatalf("targets = %+v, want one with the first platform", targets)
	}
}

func TestRun_IndexWithTooManyPlatformsIsSkippedWithNoEvidence(t *testing.T) {
	f := newFixture(t)
	var descs []ocispec.Descriptor
	for i := 0; i < maxPlatforms+1; i++ {
		d := f.imageN(ocispec.MediaTypeImageManifest, i)
		descs = append(descs, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(d.String())})
	}
	res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), f.job(f.index(descs...), "oci-index"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(res) != `{"skipped":"too many platforms"}` || len(f.ev.attached) != 0 {
		t.Fatalf("result = %s, attached = %d", res, len(f.ev.attached))
	}
}

func TestRun_IndexWithNoScannablePlatformsIsSkippedWithNoEvidence(t *testing.T) {
	f := newFixture(t)
	idx := f.index(ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(f.image(ocispec.MediaTypeImageManifest).String()),
		Annotations: map[string]string{annotationReferenceType: attestationManifest},
		Platform:    &ocispec.Platform{OS: "unknown", Architecture: "unknown"},
	})
	res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), f.job(idx, "oci-index"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if string(res) != `{"skipped":"no scannable platforms"}` || len(f.ev.attached) != 0 {
		t.Fatalf("result = %s, attached = %d", res, len(f.ev.attached))
	}
}

func TestStage_NegativeDescriptorSizeIsRefused(t *testing.T) {
	f := newFixture(t)
	cfg := f.put([]byte(`{}`))
	raw, _ := json.Marshal(ocispec.Manifest{Config: ocispec.Descriptor{Digest: godigest.Digest(cfg.String()), Size: -1}})
	_, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIManifest, f.put(raw))
	if err == nil || !strings.Contains(err.Error(), "negative size") {
		t.Fatalf("stage error = %v, want negative size", err)
	}
}

func TestRun_StagingDirectoryIsRemovedOnFailure(t *testing.T) {
	t.Run("trivy failure", func(t *testing.T) {
		f := newFixture(t)
		tmp := t.TempDir()
		_, err := f.scannerEnv(Options{Trivy: stubPath(t), TempDir: tmp}, "TRIVY_STUB_FAIL=1").Handle(t.Context(), f.job(f.image(ocispec.MediaTypeImageManifest), "oci-manifest"))
		if err == nil {
			t.Fatal("Handle succeeded")
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("%d staging entries left", len(entries))
		}
	})
	t.Run("archive too large", func(t *testing.T) {
		f := newFixture(t)
		tmp := t.TempDir()
		digest := f.put(zipOf(t, map[string]string{"big": strings.Repeat("a", 4096)}))
		_, err := f.scanner(Options{Trivy: "/nonexistent", TempDir: tmp, MaxUnpackedBytes: 1000}).Handle(t.Context(), f.job(digest, "pypi-dist"))
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("Handle error = %v, want ErrArchiveTooLarge", err)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("%d staging entries left", len(entries))
		}
	})
}

func TestRun_DatabaseTimestampIsReadForEveryJob(t *testing.T) {
	f := newFixture(t)
	stub := stubPath(t)
	// A wrapper that reports a different DB time on each version probe.
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	wrapper := filepath.Join(dir, "trivy")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then\n n=$(cat " + counter + " 2>/dev/null || echo 0); n=$((n+1)); echo $n >" + counter +
		"\n echo \"{\\\"Version\\\":\\\"0.58.1\\\",\\\"VulnerabilityDB\\\":{\\\"UpdatedAt\\\":\\\"probe-$n\\\"}}\"\n exit 0\nfi\nexec " + stub + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := f.scanner(Options{Trivy: wrapper, CacheDir: dir})
	digest := f.image(ocispec.MediaTypeImageManifest)
	for i := 0; i < 2; i++ {
		if _, err := s.Handle(t.Context(), f.job(digest, "oci-manifest")); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	var got []string
	for _, a := range f.ev.attached {
		var m evidenceMetadata
		_ = json.Unmarshal(a.params.Metadata, &m)
		got = append(got, m.DBUpdatedAt)
	}
	if strings.Join(got, ",") != "probe-1,probe-1,probe-2,probe-2" {
		t.Fatalf("db_updated_at = %v", got)
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"UserAndPassword", "FATAL pull https://bob:s3cret@mirror.example/v2/ failed", "FATAL pull https://REDACTED@mirror.example/v2/ failed"},
		{"TokenOnly", "oci://tok@host/x", "oci://REDACTED@host/x"},
		{"NoCredentials", "get https://mirror.example/v2/: timeout", "get https://mirror.example/v2/: timeout"},
		{"AtSignInPathIsKept", "https://host/a@b", "https://host/a@b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactURLUserinfo(tc.in); got != tc.want {
				t.Errorf("redactURLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRun_TrivyErrorRedactsMirrorCredentials(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trivy")
	script := "#!/bin/sh\necho 'FATAL https://u:pw@mirror.example/db unreachable' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	_, err := (&Scanner{opts: Options{Trivy: bin}}).run(t.Context(), "", "image")
	if err == nil || strings.Contains(err.Error(), "pw") || !strings.Contains(err.Error(), "REDACTED@mirror.example") {
		t.Fatalf("err = %v, want the credentials redacted", err)
	}
}

func TestRun_TrivyRunsWithAFilteredEnvironmentInTheStagingDirectory(t *testing.T) {
	t.Setenv("TRIVY_SEVERITY", "CRITICAL")
	t.Setenv("TRIVY_CONFIG", "/etc/evil.yaml")
	t.Setenv("SINDRI_DATABASE_URL", "postgres://u:hunter2@db/sindri")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "hunter2")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("no_proxy", "localhost")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/ca.pem")
	envFile := filepath.Join(t.TempDir(), "env")
	f := newFixture(t)
	tmp := t.TempDir()
	digest := f.image(ocispec.MediaTypeImageManifest)
	s := f.scannerEnv(Options{Trivy: stubPath(t), TempDir: tmp}, "TRIVY_STUB_ENV="+envFile)
	if _, err := s.Handle(t.Context(), f.job(digest, "oci-manifest")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	recorded := string(readFile(t, envFile))
	lines := strings.Split(recorded, "\n")
	has := func(line string) bool { return slices.Contains(lines, line) }
	for _, want := range []string{"HTTPS_PROXY=http://proxy.example:3128", "no_proxy=localhost", "SSL_CERT_FILE=/etc/ssl/ca.pem"} {
		if !has(want) {
			t.Errorf("environment lacks %q:\n%s", want, recorded)
		}
	}
	for _, banned := range []string{"TRIVY_SEVERITY", "TRIVY_CONFIG", "SINDRI_DATABASE_URL", "AWS_SECRET_ACCESS_KEY", "hunter2"} {
		if strings.Contains(recorded, banned) {
			t.Errorf("environment leaks %q:\n%s", banned, recorded)
		}
	}
	resolved, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lines[0], "CWD="+resolved+string(filepath.Separator)+"sindri-scan-") {
		t.Errorf("trivy ran in %q, want a directory under %s", lines[0], resolved)
	}
}

func TestTrivyEnv_KeepsOnlyTheAllowList(t *testing.T) {
	got := trivyEnv([]string{"PATH=/bin", "TRIVY_X=1", "Http_Proxy=p", "HOME=/h", "SECRET=s", "malformed"}, []string{"EXTRA=1"})
	want := []string{"PATH=/bin", "Http_Proxy=p", "HOME=/h", "EXTRA=1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("trivyEnv = %v, want %v", got, want)
	}
}

func TestRun_StagingParentIsCreatedPrivate(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(t.TempDir(), "cache", "sindri-staging")
	if _, err := f.scanner(Options{Trivy: stubPath(t), TempDir: parent}).Handle(t.Context(), f.job(f.image(ocispec.MediaTypeImageManifest), "oci-manifest")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("staging parent mode = %v, want 0700", info.Mode().Perm())
	}
}

// imageWithLayer stores a manifest whose config differs per tag and whose
// single layer is layer, returning the manifest and layer digests.
func (f *fixture) imageWithLayer(tag string, layer []byte) (manifest, layerDigest model.Digest) {
	f.t.Helper()
	cfgBytes := []byte(`{"architecture":"amd64","os":"linux","tag":"` + tag + `"}`)
	cfg := f.put(cfgBytes)
	layerDigest = f.put(layer)
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(cfg.String()), Size: int64(len(cfgBytes))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: godigest.Digest(layerDigest.String()), Size: int64(len(layer))}},
	}
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.put(raw), layerDigest
}

func manifestDesc(d model.Digest, os, arch string) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(d.String()),
		Platform: &ocispec.Platform{OS: os, Architecture: arch}}
}

func TestRun_BlobOutsideThePublishingRepositoryIsSkippedWithNoEvidence(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture) (subject model.Digest, kind string, missing model.Digest)
	}{
		{name: "IndexChildOnlyInAnotherRepository", setup: func(f *fixture) (model.Digest, string, model.Digest) {
			own, _ := f.imageWithLayer("own", []byte("own layer"))
			foreign, _ := f.imageWithLayer("foreign", []byte("foreign layer"))
			return f.index(manifestDesc(own, "linux", "amd64"), manifestDesc(foreign, "linux", "arm64")), "oci-index", foreign
		}},
		{name: "ManifestLayerOnlyInAnotherRepository", setup: func(f *fixture) (model.Digest, string, model.Digest) {
			m, layer := f.imageWithLayer("x", []byte("foreign layer"))
			return m, "oci-manifest", layer
		}},
		{name: "PackageFileNotInRepository", setup: func(f *fixture) (model.Digest, string, model.Digest) {
			d := f.put(zipOf(t, map[string]string{"a.txt": "x"}))
			return d, "pypi-dist", d
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			subject, kind, missing := tt.setup(f)
			f.placed.deny(testRepo, missing)
			res, err := f.scanner(Options{Trivy: "/nonexistent", MaxUnpackedBytes: 1 << 20}).Handle(t.Context(), f.job(subject, kind))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			want := `{"skipped":"blob not in repository","digest":"` + missing.String() + `"}`
			if string(res) != want || len(f.ev.attached) != 0 {
				t.Fatalf("result = %s (attached %d), want %s and no evidence", res, len(f.ev.attached), want)
			}
		})
	}
}

func TestRun_JobWithoutRepositoryIsSkippedWithNoEvidence(t *testing.T) {
	f := newFixture(t)
	digest := f.image(ocispec.MediaTypeImageManifest)
	args, _ := json.Marshal(map[string]string{"digest": digest.String(), "kind": "oci-manifest"})
	res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), queue.Job{ID: 7, Kind: scanjob.JobKind, Args: args})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := `{"skipped":"blob not in repository","digest":"` + digest.String() + `"}`
	if string(res) != want || len(f.ev.attached) != 0 {
		t.Fatalf("result = %s (attached %d), want %s", res, len(f.ev.attached), want)
	}
}

func TestRun_IndexChildRules(t *testing.T) {
	tests := []struct {
		name        string
		child       func(real model.Digest) ocispec.Descriptor
		wantSkipped string
		wantTargets int
	}{
		{name: "AttestationLabelOnARealPlatformIsScanned", wantTargets: 2,
			child: func(real model.Digest) ocispec.Descriptor {
				d := manifestDesc(real, "linux", "arm64")
				d.Annotations = map[string]string{annotationReferenceType: attestationManifest}
				return d
			}},
		{name: "NestedIndexSkipsTheWholeJob", wantSkipped: `{"skipped":"unsupported child","digest":"%s","mediaType":"application/vnd.oci.image.index.v1+json"}`,
			child: func(real model.Digest) ocispec.Descriptor {
				return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: godigest.Digest(real.String())}
			}},
		{name: "UnknownMediaTypeSkipsTheWholeJob", wantSkipped: `{"skipped":"unsupported child","digest":"%s","mediaType":"application/x-weird"}`,
			child: func(real model.Digest) ocispec.Descriptor {
				return ocispec.Descriptor{MediaType: "application/x-weird", Digest: godigest.Digest(real.String())}
			}},
		{name: "UnknownUnknownAttestationIsSkipped", wantTargets: 1,
			child: func(real model.Digest) ocispec.Descriptor {
				d := manifestDesc(real, "unknown", "unknown")
				d.Annotations = map[string]string{annotationReferenceType: attestationManifest}
				return d
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			amd := f.imageN(ocispec.MediaTypeImageManifest, 1)
			other := f.imageN(ocispec.MediaTypeImageManifest, 2)
			idx := f.index(manifestDesc(amd, "linux", "amd64"), tt.child(other))
			if tt.wantSkipped != "" {
				res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), f.job(idx, "oci-index"))
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if want := strings.Replace(tt.wantSkipped, "%s", other.String(), 1); string(res) != want || len(f.ev.attached) != 0 {
					t.Fatalf("result = %s (attached %d), want %s", res, len(f.ev.attached), want)
				}
				return
			}
			targets, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIIndex, idx)
			if err != nil {
				t.Fatalf("stage: %v", err)
			}
			if len(targets) != tt.wantTargets {
				t.Fatalf("targets = %+v, want %d", targets, tt.wantTargets)
			}
		})
	}
}

func TestStage_IndexPlatformsSharingALayerStageItOnce(t *testing.T) {
	f := newFixture(t)
	shared := bytes.Repeat([]byte("shared layer "), 1<<14)
	amd, layer := f.imageWithLayer("amd", shared)
	arm, _ := f.imageWithLayer("arm", shared)
	idx := f.index(manifestDesc(amd, "linux", "amd64"), manifestDesc(arm, "linux", "arm64"))

	targets, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIIndex, idx)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	var infos []os.FileInfo
	for _, tg := range targets {
		fi, err := os.Stat(filepath.Join(tg.dir, "blobs", "sha256", layer.Hex()))
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, fi)
	}
	if !os.SameFile(infos[0], infos[1]) {
		t.Fatal("the shared layer was written once per platform, want one file linked into both layouts")
	}
}

func TestRun_IndexWithTwoChildrenOnOnePlatformIsSkippedWithNoEvidence(t *testing.T) {
	f := newFixture(t)
	a := f.imageN(ocispec.MediaTypeImageManifest, 1)
	b := f.imageN(ocispec.MediaTypeImageManifest, 2)
	idx := f.index(manifestDesc(a, "linux", "amd64"), manifestDesc(b, "linux", "amd64"))
	res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), f.job(idx, "oci-index"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if want := `{"skipped":"duplicate platform","platform":"linux/amd64"}`; string(res) != want || len(f.ev.attached) != 0 {
		t.Fatalf("result = %s (attached %d), want %s", res, len(f.ev.attached), want)
	}
}

func TestStage_WindowsChildrenDifferingByOSVersionAreBothStaged(t *testing.T) {
	f := newFixture(t)
	a := f.imageN(ocispec.MediaTypeImageManifest, 1)
	b := f.imageN(ocispec.MediaTypeImageManifest, 2)
	da, db2 := manifestDesc(a, "windows", "amd64"), manifestDesc(b, "windows", "amd64")
	da.Platform.OSVersion, db2.Platform.OSVersion = "10.0.17763", "10.0.20348"
	da.Platform.OSFeatures = []string{"win32k", "b"}
	targets, err := f.scanner(Options{}).stage(t.Context(), testRepo, t.TempDir(), model.SubjectKindOCIIndex, f.index(da, db2))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	var got []string
	for _, tg := range targets {
		got = append(got, tg.platform)
	}
	if want := "windows/amd64:10.0.17763+b+win32k,windows/amd64:10.0.20348"; strings.Join(got, ",") != want {
		t.Fatalf("platforms = %v, want %s", got, want)
	}
}

func TestRun_ChildDescribedAsManifestButAnIndexIsSkippedWithNoEvidence(t *testing.T) {
	f := newFixture(t)
	// A manifest-shaped body declaring an index media type.
	raw, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageIndex})
	liar := f.put(raw)
	idx := f.index(manifestDesc(liar, "linux", "amd64"))
	res, err := f.scanner(Options{Trivy: "/nonexistent"}).Handle(t.Context(), f.job(idx, "oci-index"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := `{"skipped":"unsupported child","digest":"` + liar.String() + `","mediaType":"application/vnd.oci.image.index.v1+json"}`
	if string(res) != want || len(f.ev.attached) != 0 {
		t.Fatalf("result = %s (attached %d), want %s", res, len(f.ev.attached), want)
	}
}
