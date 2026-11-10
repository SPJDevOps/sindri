//go:build integration

package scan

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/testsupport"
)

// apkInstalled is an Alpine package database naming one package with known
// CVEs. Trivy needs nothing more than this and /etc/alpine-release to
// recognise the operating system and its packages.
const apkInstalled = `C:Q1ZbXjtYBZc5Zx6KRuQSBrqdbTwro=
P:openssl
V:1.1.1a-r1
A:x86_64
S:1
I:1
T:Toolkit for SSL v1/v2/v3 and TLS v1
U:https://www.openssl.org
L:OpenSSL
o:openssl
m:Test <test@example.com>
t:1546300800
c:0000000000000000000000000000000000000000

`

func realImageLayer(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"etc/alpine-release", "3.10.0\n"},
		{"etc/os-release", "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.10.0\n"},
		{"lib/apk/db/installed", apkInstalled},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestRealTrivy_ScansAnImageWithAKnownVulnerablePackage runs the Scanner
// against the real Trivy, so the argv, the report shape, the database lookup
// and the sbom conversion are checked against the program rather than the
// stub. It needs the network for Trivy's vulnerability database; set
// SINDRI_TRIVY_CACHE to a directory to keep the download between runs.
func TestRealTrivy_ScansAnImageWithAKnownVulnerablePackage(t *testing.T) {
	trivy, cacheDir := realTrivy(t)
	f := newFixture(t)
	layerBytes := realImageLayer(t)
	layer := f.put(layerBytes)
	cfgBytes, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{Architecture: "amd64", OS: "linux"},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []godigest.Digest{godigest.Digest(layer.String())}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.put(cfgBytes)
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(cfg.String()), Size: int64(len(cfgBytes))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer, Digest: godigest.Digest(layer.String()), Size: int64(len(layerBytes))}},
	}
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	digest := f.put(raw)

	sbom, scan := realScan(t, f, trivy, cacheDir, digest, model.SubjectKindOCIManifest)

	var meta struct {
		Producer       string         `json:"producer"`
		Scanner        string         `json:"scanner"`
		ScannerVersion string         `json:"scanner_version"`
		DBUpdatedAt    string         `json:"db_updated_at"`
		Coverage       string         `json:"coverage"`
		Counts         map[string]int `json:"severity_counts"`
	}
	if err := json.Unmarshal(scan.params.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Producer != "sindri" || meta.Scanner != "trivy" || meta.Coverage != "image" || meta.ScannerVersion == "" || meta.DBUpdatedAt == "" {
		t.Errorf("metadata = %+v", meta)
	}
	total := 0
	for _, n := range meta.Counts {
		total += n
	}
	if total == 0 {
		t.Errorf("severity_counts = %v, want findings for openssl 1.1.1a", meta.Counts)
	}

	var report struct {
		Results []struct {
			Class           string
			Vulnerabilities []struct{ VulnerabilityID, PkgName string }
			Packages        []struct{ Name string }
		}
	}
	if err := json.Unmarshal(scan.body, &report); err != nil {
		t.Fatalf("scan evidence is not a Trivy report: %v", err)
	}
	var ids []string
	for _, r := range report.Results {
		for _, v := range r.Vulnerabilities {
			if v.PkgName == "openssl" {
				ids = append(ids, v.VulnerabilityID)
			}
		}
	}
	t.Logf("openssl findings: %v", ids)
	if !slices.Contains(ids, "CVE-2019-1543") {
		t.Errorf("report has no CVE-2019-1543 for openssl 1.1.1a; findings = %v", ids)
	}
	if !bytes.Contains(sbom.body, []byte(`"bomFormat": "CycloneDX"`)) && !bytes.Contains(sbom.body, []byte(`"bomFormat":"CycloneDX"`)) {
		t.Error("sbom evidence is not CycloneDX")
	}
	if !bytes.Contains(sbom.body, []byte("openssl")) {
		t.Error("sbom does not list openssl")
	}
}

// realTrivy returns the Trivy executable and a prepared cache directory.
func realTrivy(t *testing.T) (trivy, cacheDir string) {
	t.Helper()
	trivy = testsupport.TrivyBinary(t)
	cacheDir = os.Getenv("SINDRI_TRIVY_CACHE")
	if cacheDir == "" {
		cacheDir = t.TempDir()
	}
	if err := PrepareCacheDir(cacheDir); err != nil {
		t.Fatal(err)
	}
	return trivy, cacheDir
}

// realScan runs the Scanner on digest and returns the sbom and scan it
// attached. A failure to download the database skips the test rather than
// failing it, unless CI is set: that is the network, not Sindri, but CI must
// not pass without having scanned anything.
func realScan(t *testing.T, f *fixture, trivy, cacheDir string, digest model.Digest, kind model.SubjectKind) (sbom, scan attachment) {
	t.Helper()
	s := f.scanner(Options{Trivy: trivy, CacheDir: cacheDir, Timeout: 10 * time.Minute, MaxUnpackedBytes: 1 << 30})
	if _, err := s.Handle(t.Context(), f.job(digest, string(kind))); err != nil {
		if strings.Contains(err.Error(), "DB") || strings.Contains(err.Error(), "download") || strings.Contains(err.Error(), "TOOMANYREQUESTS") {
			if os.Getenv("CI") != "" {
				t.Fatalf("trivy could not fetch its vulnerability database: %v", err)
			}
			t.Skipf("trivy could not fetch its vulnerability database (network?): %v", err)
		}
		t.Fatalf("Handle: %v", err)
	}
	if len(f.ev.attached) != 2 {
		t.Fatalf("attached %d, want sbom and scan", len(f.ev.attached))
	}
	return f.ev.attached[0], f.ev.attached[1]
}

// An npm tarball goes through `trivy rootfs`, the other half of the scanner.
// The package bundles node_modules/minimist 0.0.8, which has known CVEs.
func TestRealTrivy_ScansAnNPMTarballWithABundledVulnerablePackage(t *testing.T) {
	trivy, cacheDir := realTrivy(t)
	f := newFixture(t)
	digest := f.put(tarGz(t, []tar.Header{
		{Name: "package/package.json", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "package/node_modules/minimist/package.json", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{
		"package/package.json":                       `{"name":"bundler","version":"1.0.0"}`,
		"package/node_modules/minimist/package.json": `{"name":"minimist","version":"0.0.8"}`,
	}))
	sbom, scan := realScan(t, f, trivy, cacheDir, digest, model.SubjectKindNPMTarball)

	var meta struct {
		Producer string         `json:"producer"`
		Coverage string         `json:"coverage"`
		Counts   map[string]int `json:"severity_counts"`
	}
	if err := json.Unmarshal(scan.params.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Producer != "sindri" || meta.Coverage != "package" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.Counts["CRITICAL"]+meta.Counts["HIGH"]+meta.Counts["MEDIUM"]+meta.Counts["LOW"] == 0 {
		t.Errorf("severity_counts = %v, want findings for minimist 0.0.8", meta.Counts)
	}
	if !bytes.Contains(scan.body, []byte("minimist")) || !bytes.Contains(sbom.body, []byte("minimist")) {
		t.Error("evidence does not mention the bundled minimist")
	}
}
