package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// stderrTail is how much of Trivy's stderr an error message keeps.
const stderrTail = 4 << 10

const (
	// versionTimeout bounds the version probe.
	versionTimeout = 30 * time.Second
	// waitDelay bounds how long Wait lingers on I/O after Trivy is killed.
	waitDelay = 10 * time.Second
)

// trivyVersion is what `trivy --version --format json` reports.
type trivyVersion struct {
	Version string `json:"Version"`
	DB      struct {
		UpdatedAt string `json:"UpdatedAt"`
	} `json:"VulnerabilityDB"`
}

// tailBuffer keeps the last stderrTail bytes written to it.
type tailBuffer struct{ buf []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-stderrTail:]...)
	}
	return len(p), nil
}

// userinfoRE matches the credentials in scheme://user:pass@host.
var userinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/@]+@`)

// redactURLUserinfo hides URL credentials, such as those of a configured
// mirror that Trivy echoes in an error, before the text is stored in a job.
func redactURLUserinfo(s string) string { return userinfoRE.ReplaceAllString(s, "${1}REDACTED@") }

// passedEnv names the environment variables Trivy inherits; anything else is
// dropped. A name matches case-insensitively, which covers the lowercase proxy
// variants some tools read.
var passedEnv = map[string]bool{
	"PATH": true, "HOME": true, "XDG_CACHE_HOME": true, "TMPDIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

// trivyEnv returns the environment Trivy runs with: the entries of environ
// whose names are in passedEnv, then extra. It is deliberately a short
// allow-list. The evidence a scan produces claims producer "sindri", so an
// operator's TRIVY_* variables or trivy.yaml (severity filters, ignore files,
// skipped scanners, an alternative database) must not change the counts it
// records. And the server's own environment holds secrets (database URL, S3
// and OIDC credentials) that have no business reaching a third-party binary.
// TRIVY_* is never passed; trivy.yaml is not found because the working
// directory is the job's staging directory.
func trivyEnv(environ, extra []string) []string {
	out := make([]string, 0, len(passedEnv)+len(extra))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && passedEnv[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// run executes Trivy with args in dir, the job's staging directory, and
// returns its stdout. The error carries the tail of stderr, never the report.
func (s *Scanner) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// #nosec G204 -- the executable is the operator's configured trivy path; the arguments are fixed flags and paths of directories this process created.
	cmd := exec.CommandContext(ctx, s.opts.Trivy, args...)
	cmd.Dir = dir
	cmd.Env = trivyEnv(os.Environ(), s.extraEnv)
	cmd.WaitDelay = waitDelay
	var stdout bytes.Buffer
	var stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running trivy %s: %w: %s", args[0], err, redactURLUserinfo(strings.TrimSpace(string(stderr.buf))))
	}
	return stdout.Bytes(), nil
}

// versionInfo reports the scanner version and the vulnerability database
// time by asking Trivy, for every job: the database timestamp is part of the
// scan's point-in-time claim, so it is never cached. Only the version is
// remembered, as a fallback when a later probe fails. A failed probe leaves
// the fields empty and does not fail the scan; the next job probes again.
func (s *Scanner) versionInfo(ctx context.Context, dir string) trivyVersion {
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), versionTimeout)
	defer cancel()
	args := []string{"--version", "--format", "json"}
	if s.opts.CacheDir != "" {
		args = append(args, "--cache-dir", s.opts.CacheDir)
	}
	var v trivyVersion
	out, err := s.run(probeCtx, dir, args...)
	if err == nil {
		err = json.Unmarshal(out, &v)
	}
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	if err != nil {
		s.logger.WarnContext(ctx, "could not read trivy version", "error", err)
		return trivyVersion{Version: s.scannerVersion}
	}
	s.scannerVersion = v.Version
	return v
}

// scanTarget runs Trivy on t and writes report.json and sbom.cdx.json into
// outDir, returning their paths.
func (s *Scanner) scanTarget(ctx context.Context, t target, outDir string) (report, sbom string, err error) {
	if s.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.Timeout)
		defer cancel()
	}
	report = filepath.Join(outDir, "report.json")
	sbom = filepath.Join(outDir, "sbom.cdx.json")

	var args []string
	if t.coverage == "image" {
		args = []string{"image", "--input", t.dir}
	} else {
		args = []string{"rootfs", t.dir}
	}
	args = append(args, "--format", "json", "--list-all-pkgs", "--scanners", "vuln", "--quiet")
	if s.opts.CacheDir != "" {
		args = append(args, "--cache-dir", s.opts.CacheDir)
	}
	if s.opts.DBRepository != "" {
		args = append(args, "--db-repository", s.opts.DBRepository)
	}
	if s.opts.JavaDBRepository != "" {
		args = append(args, "--java-db-repository", s.opts.JavaDBRepository)
	}
	if s.opts.SkipDBUpdate {
		// The Java database is a second download, made only when an image or
		// package holds a jar. Without this flag a "never download" server
		// would reach out for it. Trivy then needs it already in the cache.
		args = append(args, "--skip-db-update", "--skip-java-db-update")
	}
	args = append(args, "--output", report)
	if _, err := s.run(ctx, outDir, args...); err != nil {
		return "", "", err
	}
	if _, err := s.run(ctx, outDir, "convert", "--format", "cyclonedx", "--output", sbom, report); err != nil {
		return "", "", err
	}
	return report, sbom, nil
}
