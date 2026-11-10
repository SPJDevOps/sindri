package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// stderrTail is how much of Trivy's stderr an error message keeps.
const stderrTail = 4 << 10

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

// run executes Trivy with args and returns its stdout. The error carries the
// tail of stderr, never the report.
func (s *Scanner) run(ctx context.Context, args ...string) ([]byte, error) {
	// #nosec G204 -- the executable is the operator's configured trivy path; the arguments are fixed flags and paths of directories this process created.
	cmd := exec.CommandContext(ctx, s.opts.Trivy, args...)
	var stdout bytes.Buffer
	var stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running trivy %s: %w: %s", args[0], err, strings.TrimSpace(string(stderr.buf)))
	}
	return stdout.Bytes(), nil
}

// versionInfo reports the scanner version and vulnerability database time,
// asking Trivy once. A failure leaves both empty and does not fail a scan.
func (s *Scanner) versionInfo(ctx context.Context) trivyVersion {
	s.versionOnce.Do(func() {
		out, err := s.run(ctx, "--version", "--format", "json")
		if err != nil {
			s.logger.Warn("could not read trivy version", "error", err)
			return
		}
		var v trivyVersion
		if err := json.Unmarshal(out, &v); err != nil {
			s.logger.Warn("could not decode trivy version", "error", err)
			return
		}
		s.version = v
	})
	return s.version
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
	if s.opts.SkipDBUpdate {
		args = append(args, "--skip-db-update")
	}
	args = append(args, "--output", report)
	if _, err := s.run(ctx, args...); err != nil {
		return "", "", err
	}
	if _, err := s.run(ctx, "convert", "--format", "cyclonedx", "--output", sbom, report); err != nil {
		return "", "", err
	}
	return report, sbom, nil
}
