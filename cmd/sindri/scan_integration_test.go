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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/spj/sindri/internal/storage/memstore"
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
	return startScanWith(t, dbURL, enabled, trivyStub(t), extraRepos)
}

func trivyStub(t *testing.T) string {
	t.Helper()
	stub, err := filepath.Abs("../../internal/scan/testdata/trivy-stub.sh")
	if err != nil {
		t.Fatal(err)
	}
	return stub
}

// failingTrivyStub is the stub told to fail. The scanner gives Trivy a
// filtered environment, so the switch has to live in the executable itself.
func failingTrivyStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trivy")
	script := "#!/bin/sh\nTRIVY_STUB_FAIL=1 exec " + trivyStub(t) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	return path
}

func startScanWith(t *testing.T, dbURL string, enabled bool, stub, extraRepos string) *stack {
	t.Helper()
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
	db := testsupport.DatabaseURL(t)
	s := startScanWith(t, db, true, failingTrivyStub(t), "")
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

// rescanStub is the Trivy stub with a switch. The scanner gives Trivy a
// filtered environment, so the switch lives in the wrapper: while the returned
// file holds a fixture name, image scans emit that fixture, which simulates a
// vulnerability database that learned a new CVE after the first scan.
func rescanStub(t *testing.T) (stub string, newerDatabase func()) {
	t.Helper()
	dir := t.TempDir()
	report := filepath.Join(dir, "report")
	stub = filepath.Join(dir, "trivy")
	script := "#!/bin/sh\nTRIVY_STUB_REPORT_FILE=" + report + " exec " + trivyStub(t) + " \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	return stub, func() {
		t.Helper()
		if err := os.WriteFile(report, []byte("report-image-newer.json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// rescanStack runs serve with scanning on, the given rescanInterval, one hosted
// OCI repository, and a dev stage that has no scan gate.
func rescanStack(t *testing.T, dbURL, stub, interval string, blobs *memstore.Store) *stack {
	t.Helper()
	body := fmt.Sprintf("scanning:\n  enabled: true\n  trivy: %q\n  cacheDir: %q\n  rescanInterval: %s\n"+
		"repos:\n  docker: { protocol: oci }\nlifecycle:\n  stages: [dev]\n", stub, t.TempDir(), interval)
	return startServeOn(t, "127.0.0.1:0", dbURL, "", body, func(h *serveHooks) {
		scanHooks(h)
		h.build.Blobs = blobs
	})
}

const rescanNewCVE = "CVE-2026-0099"

// scans returns the Sindri scan evidence rows of a subject.
func (s *stack) scans(subject string) []evidenceItem {
	s.t.Helper()
	var out []evidenceItem
	_, items := s.evidence(subject)
	for _, it := range items {
		if it.Type == "scan" {
			out = append(out, it)
		}
	}
	return out
}

// promoteTo approves a subject at a stage.
func (s *stack) promoteTo(subject, stage string) {
	s.t.Helper()
	r := s.request(http.MethodPost, "/api/v1/subjects/"+subject+"/promote",
		strings.NewReader(fmt.Sprintf(`{"repository":"docker","stage":%q,"reason":"rescan test"}`, stage)), bearer(bootstrapToken))
	if r.status != http.StatusCreated {
		s.t.Fatalf("promote: %d %s", r.status, r.body)
	}
}

// ledger is everything the lifecycle ledger shows an operator.
func (s *stack) ledger(subject string) string {
	s.t.Helper()
	events := s.request(http.MethodGet, "/api/v1/subjects/"+subject+"/events", nil, bearer(bootstrapToken))
	audit := s.request(http.MethodGet, "/api/v1/audit", nil, bearer(bootstrapToken))
	if events.status != http.StatusOK || audit.status != http.StatusOK {
		s.t.Fatalf("ledger: events %d, audit %d", events.status, audit.status)
	}
	return string(events.body) + "\n" + string(audit.body)
}

// awaitNewFindings waits for a second scan and returns the ids it lists as new.
func (s *stack) awaitNewFindings(subject string) []string {
	s.t.Helper()
	var scans []evidenceItem
	eventually(s.t, "the second scan to be attached", func() bool {
		scans = s.scans(subject)
		return len(scans) >= 2
	})
	var withField []evidenceItem
	for _, sc := range scans {
		if _, ok := sc.Metadata["new_vulnerabilities"]; ok {
			withField = append(withField, sc)
		}
	}
	if len(scans) != 2 || len(withField) != 1 {
		s.t.Fatalf("scans = %d, with new_vulnerabilities = %d; want 2 and 1 (the first scan has no previous one)", len(scans), len(withField))
	}
	var ids []string
	if err := json.Unmarshal(withField[0].Metadata["new_vulnerabilities"], &ids); err != nil {
		s.t.Fatalf("new_vulnerabilities: %v", err)
	}
	return ids
}

func (s *stack) assertRescanShown(subject string, before string) {
	s.t.Helper()
	if after := s.ledger(subject); after != before {
		s.t.Errorf("the ledger changed during a re-scan:\nbefore: %s\nafter:  %s", before, after)
	}
	eventually(s.t, "the gauge to count the approved subject with a CRITICAL finding", func() bool {
		return strings.Contains(string(s.request(http.MethodGet, "/metrics", nil, nil).body), `sindri_approved_findings{severity="CRITICAL"} 1`)
	})
	status, page, _ := visit(s.t, browser(s.t, true), http.MethodGet,
		s.base+"/ui/r/docker/-/artifact?image=demo%2Fapp&digest="+subject, bearer(bootstrapToken))
	if status != http.StatusOK {
		s.t.Fatalf("artifact page: %d %s", status, page)
	}
	if !strings.Contains(page, rescanNewCVE+` <span class="pill new">new</span>`) {
		s.t.Errorf("the artifact page does not mark %s as new:\n%s", rescanNewCVE, page)
	}
	if strings.Contains(page, "CVE-2026-0001 <span class=\"pill new\">") {
		s.t.Error("the artifact page marks a finding of the first scan as new")
	}
}

// The M11b gate through the real wiring: a database that learns a CVE flags an
// approved image on the next scan, and the ledger is left exactly as it was.
func TestScan_RescanFlagsAnApprovedSubjectWithoutChangingTheLedger(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, newerDatabase := rescanStub(t)
	s := rescanStack(t, db, stub, "0s", memstore.New())

	digest := s.push("docker/demo/app", "1.0", "rescan-on-demand")
	eventually(t, "the first scan", func() bool { return len(s.scans(digest)) == 1 })
	s.promoteTo(digest, "dev")
	before := s.ledger(digest)

	newerDatabase()
	r := s.request(http.MethodPost, "/api/v1/subjects/"+digest+"/scan", nil, bearer(bootstrapToken))
	if r.status != http.StatusAccepted {
		t.Fatalf("POST scan: %d %s, want 202", r.status, r.body)
	}
	if got := s.awaitNewFindings(digest); len(got) != 1 || got[0] != rescanNewCVE {
		t.Fatalf("new_vulnerabilities = %v, want [%s]", got, rescanNewCVE)
	}
	s.assertRescanShown(digest, before)
}

// The schedule alone does the same. The first run of the schedule is due at the
// worker's first tick because nothing has succeeded yet, so the test lets one
// serve scan and approve the image with the schedule off, then restarts serve
// on the same database and blobs with the schedule on.
func TestScan_ScheduledRescanFlagsAnApprovedSubjectWithoutChangingTheLedger(t *testing.T) {
	db := testsupport.DatabaseURL(t)
	stub, newerDatabase := rescanStub(t)
	blobs := memstore.New()
	first := rescanStack(t, db, stub, "0s", blobs)

	digest := first.push("docker/demo/app", "1.0", "rescan-scheduled")
	eventually(t, "the first scan", func() bool { return len(first.scans(digest)) == 1 })
	first.promoteTo(digest, "dev")
	first.stop()

	newerDatabase()
	s := rescanStack(t, db, stub, "1m", blobs)
	before := s.ledger(digest)
	if got := s.awaitNewFindings(digest); len(got) != 1 || got[0] != rescanNewCVE {
		t.Fatalf("new_vulnerabilities = %v, want [%s]", got, rescanNewCVE)
	}
	s.assertRescanShown(digest, before)
}
