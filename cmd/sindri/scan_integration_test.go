//go:build integration

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // G505: npm's dist.shasum, which the publish document must carry
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/spj/sindri/internal/testsupport"
)

const (
	scanOCIRepo   = "docker"
	scanPyPIRepo  = "py"
	scanNPMRepo   = "npm"
	scanProxyRepo = "npmjs"
)

// startScan runs serve with scanning on or off, with scanning with the unit-test stub standing in for Trivy,
// which answers with fixtures whose severity counts these tests assert.
func startScan(t *testing.T, dbURL string, enabled bool, extraRepos string) *stack {
	t.Helper()
	stub, err := filepath.Abs("../../internal/scan/testdata/trivy-stub.sh")
	if err != nil {
		t.Fatal(err)
	}
	// npm repositories need publicUrl, which must name the bound address.
	addr := freeAddr(t)
	body := fmt.Sprintf("publicUrl: http://%s\nscanning:\n  enabled: %t\n  trivy: %q\n  cacheDir: %q\n%s%s",
		addr, enabled, stub, t.TempDir(), scanRepos, extraRepos)
	return startServeOn(t, addr, dbURL, "", body, scanHooks)
}

const scanRepos = `repos:
  docker: { protocol: oci }
  py: { protocol: pypi }
  npm: { protocol: npm }
`

func scanHooks(h *serveHooks) {
	fastWorker(h)
	h.build.WorkerBackoff = 100 * time.Millisecond
}

// scanJob is a row of the jobs table.
type scanJob struct {
	ID       int64
	State    string
	Attempts int
	Error    string
}

// scanJobs reads the scan jobs straight from the database: the API can fetch
// a job only by id, and these tests are about jobs nobody has been told of.
func scanJobs(t *testing.T, dbURL string) []scanJob {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	rows, err := conn.Query(t.Context(), `SELECT id, state, attempts, coalesce(last_error, '') FROM jobs WHERE kind = 'scan' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []scanJob
	for rows.Next() {
		var j scanJob
		if err := rows.Scan(&j.ID, &j.State, &j.Attempts, &j.Error); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type evidenceItem struct {
	Type      string                     `json:"type"`
	MediaType string                     `json:"media_type"`
	CreatedBy string                     `json:"created_by"`
	Metadata  map[string]json.RawMessage `json:"metadata"`
}

func (s *stack) evidence(subject string) (int, []evidenceItem) {
	s.t.Helper()
	r := s.request(http.MethodGet, "/api/v1/subjects/"+subject+"/evidence", nil, bearer(bootstrapToken))
	if r.status != http.StatusOK {
		return r.status, nil
	}
	var page struct {
		Items []evidenceItem `json:"items"`
	}
	if err := json.Unmarshal(r.body, &page); err != nil {
		s.t.Fatal(err)
	}
	return r.status, page.Items
}

// awaitScanEvidence waits for the scan job's sbom and scan, then checks
// everything the severity gate will rely on.
func (s *stack) awaitScanEvidence(subject, coverage string) {
	s.t.Helper()
	var items []evidenceItem
	eventually(s.t, "the scan evidence to be attached", func() bool {
		_, items = s.evidence(subject)
		return len(items) >= 2
	})
	if len(items) != 2 {
		s.t.Fatalf("evidence = %d items, want one sbom and one scan", len(items))
	}
	var sbom, scan *evidenceItem
	for i := range items {
		switch items[i].Type {
		case "sbom":
			sbom = &items[i]
		case "scan":
			scan = &items[i]
		}
	}
	if sbom == nil || scan == nil {
		s.t.Fatalf("evidence = %+v, want one sbom and one scan", items)
	}
	if sbom.MediaType != "application/vnd.cyclonedx+json" || scan.MediaType != "application/vnd.aquasec.trivy.report+json" {
		s.t.Errorf("media types = %s, %s", sbom.MediaType, scan.MediaType)
	}
	for _, it := range items {
		if it.CreatedBy != "system:scanner" {
			s.t.Errorf("%s created_by = %s, want system:scanner", it.Type, it.CreatedBy)
		}
		if got := string(it.Metadata["producer"]); got != `"sindri"` {
			s.t.Errorf("%s producer = %s", it.Type, got)
		}
		if got := string(it.Metadata["coverage"]); got != strconvQuote(coverage) {
			s.t.Errorf("%s coverage = %s, want %s", it.Type, got, coverage)
		}
	}
	var counts map[string]int
	if err := json.Unmarshal(scan.Metadata["severity_counts"], &counts); err != nil {
		s.t.Fatalf("severity_counts: %v", err)
	}
	if counts["CRITICAL"]+counts["HIGH"]+counts["MEDIUM"] == 0 {
		s.t.Errorf("severity_counts = %v, want the fixture's findings", counts)
	}
}

func strconvQuote(v string) string { return `"` + v + `"` }

func TestScan_HostedImagePushAttachesSbomAndScan(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	s := startScan(t, db, true, "")
	digest := s.push("docker/demo/app", "1.0", "scan-image")
	s.awaitScanEvidence(digest, "image")

	_, items := s.evidence(digest)
	for _, it := range items {
		if it.Type == "scan" {
			var got map[string]int
			if err := json.Unmarshal(it.Metadata["severity_counts"], &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]int{"CRITICAL": 1, "HIGH": 1, "MEDIUM": 0, "LOW": 0, "UNKNOWN": 0}
			if !maps.Equal(got, want) {
				t.Errorf("severity_counts = %v, want the image fixture's %v", got, want)
			}
		}
	}
}

func TestScan_MetricsCountScanAttempts(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	s := startScan(t, db, true, "")
	digest := s.push("docker/demo/app", "1.0", "scan-metrics")
	s.awaitScanEvidence(digest, "image")

	eventually(t, "the scan to be counted", func() bool {
		body := string(s.request(http.MethodGet, "/metrics", nil, nil).body)
		return strings.Contains(body, `sindri_scans_total{result="succeeded"} 1`) &&
			strings.Contains(body, `sindri_scans_total{result="failed"} 0`) &&
			strings.Contains(body, "sindri_scan_duration_seconds_count 1")
	})
}

func TestScan_HostedWheelAttachesSbomAndScan(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	s := startScan(t, db, true, "")

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, body := range map[string]string{
		"scanpkg-1.0.0.dist-info/METADATA": "Name: scanpkg\nVersion: 1.0.0\n",
		"scanpkg/__init__.py":              "x = 1\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	wheel := zbuf.Bytes()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("name", "scanpkg")
	_ = mw.WriteField("version", "1.0.0")
	part, err := mw.CreateFormFile("content", "scanpkg-1.0.0-py3-none-any.whl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(wheel)
	_ = mw.Close()
	r := s.request(http.MethodPost, "/pypi/"+scanPyPIRepo+"/upload", &body, func(req *http.Request) {
		req.SetBasicAuth("token", bootstrapToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())
	})
	if r.status != http.StatusOK && r.status != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}
	s.awaitScanEvidence(sha(wheel), "package")
}

// gzipTar builds an npm tarball holding a package.json.
func gzipTar(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":%q,"version":%q}`, name, version),
		"package/index.js":     "module.exports = 1\n",
	}
	for n, c := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(c))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func npmIntegrity(tgz []byte) (integrity, shasum string) {
	s512 := sha512.Sum512(tgz)
	s1 := sha1.Sum(tgz) //nolint:gosec // G401: npm's dist.shasum
	return "sha512-" + base64.StdEncoding.EncodeToString(s512[:]), hex.EncodeToString(s1[:])
}

func TestScan_HostedNpmTarballAttachesSbomAndScan(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	s := startScan(t, db, true, "")

	const name, version = "scanned", "1.0.0"
	tgz := gzipTar(t, name, version)
	integrity, shasum := npmIntegrity(tgz)
	doc, err := json.Marshal(map[string]any{
		"_id": name, "name": name,
		"dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{
			"_id": name + "@" + version, "name": name, "version": version,
			"dist": map[string]string{"integrity": integrity, "shasum": shasum, "tarball": "http://client.guess/" + name + "-" + version + ".tgz"},
		}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := s.request(http.MethodPut, "/npm/"+scanNPMRepo+"/"+name, bytes.NewReader(doc), bearer(bootstrapToken))
	if r.status != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.status, r.body)
	}
	s.awaitScanEvidence(sha(tgz), "package")
}

func TestScan_ProxyContentIsNotScanned(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	up := newNpmUpstream(t)
	repos := fmt.Sprintf("  %s:\n    protocol: npm\n    upstream: { url: %q }\n", scanProxyRepo, up.srv.URL)
	s := startScan(t, db, true, repos)

	const name, version = "left-pad", "1.3.0"
	tgz := gzipTar(t, name, version)
	integrity, _ := npmIntegrity(tgz)
	up.addVersion("/"+name, name, version, tgz, map[string]any{"integrity": integrity})
	up.setLatest("/"+name, version)

	r := s.request(http.MethodGet, "/npm/"+scanProxyRepo+"/"+name, nil, bearer(bootstrapToken))
	if r.status != http.StatusOK {
		t.Fatalf("packument: %d %s", r.status, r.body)
	}
	r = s.request(http.MethodGet, "/npm/"+scanProxyRepo+"/"+name+"/-/"+name+"-"+version+".tgz", nil, bearer(bootstrapToken))
	if r.status != http.StatusOK || !bytes.Equal(r.body, tgz) {
		t.Fatalf("tarball: %d, %d bytes, want 200 and the %d upstream bytes", r.status, len(r.body), len(tgz))
	}

	time.Sleep(500 * time.Millisecond) // several worker polls
	if jobs := scanJobs(t, db); len(jobs) != 0 {
		t.Fatalf("scan jobs = %+v; proxied content must never be scanned", jobs)
	}
	if status, _ := s.evidence(sha(tgz)); status != http.StatusNotFound {
		t.Fatalf("evidence for proxied bytes = %d, want 404 (no subject)", status)
	}
}

func TestScan_DisabledEnqueuesNothing(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	s := startScan(t, db, false, "")
	digest := s.push("docker/demo/app", "1.0", "scan-disabled")

	time.Sleep(500 * time.Millisecond)
	if jobs := scanJobs(t, db); len(jobs) != 0 {
		t.Fatalf("scan jobs = %+v with scanning off", jobs)
	}
	if _, items := s.evidence(digest); len(items) != 0 {
		t.Fatalf("evidence = %+v with scanning off", items)
	}
}

func TestScan_FailingTrivyIsRetried(t *testing.T) {
	t.Setenv("TRIVY_STUB_FAIL", "1")
	db := testsupport.DatabaseURL(t)
	s := startScan(t, db, true, "")
	digest := s.push("docker/demo/app", "1.0", "scan-failing")

	// last_error is cleared while an attempt runs, so wait for a moment when
	// a later attempt has started and an earlier failure is on record.
	eventually(t, "the scan job to be retried with its failure recorded", func() bool {
		jobs := scanJobs(t, db)
		return len(jobs) == 1 && jobs[0].Attempts > 1 && jobs[0].Error != ""
	})
	if _, items := s.evidence(digest); len(items) != 0 {
		t.Fatalf("evidence = %+v after failing scans; a failed scan must leave none", items)
	}
}
