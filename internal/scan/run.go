package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
)

// evidenceMetadata is what the worker writes on Sindri's own evidence. Only
// the worker sets producer; the API stores {} (ADR 29).
type evidenceMetadata struct {
	Producer       string         `json:"producer"`
	Scanner        string         `json:"scanner"`
	ScannerVersion string         `json:"scanner_version"`
	DBUpdatedAt    string         `json:"db_updated_at"`
	SeverityCounts map[string]int `json:"severity_counts"`
	// Vulnerabilities is what the severity gate reads, so it opens no blob
	// inside the promotion transaction (ADR 30). Always present.
	Vulnerabilities []vulnerability `json:"vulnerabilities"`
	Coverage        string          `json:"coverage"`
	Platform        string          `json:"platform,omitempty"`
}

type targetResult struct {
	Platform       string         `json:"platform,omitempty"`
	SBOMID         string         `json:"sbom_id"`
	ScanID         string         `json:"scan_id"`
	SeverityCounts map[string]int `json:"severity_counts"`
}

type scanned struct {
	t      target
	report string
	sbom   string
	counts map[string]int
	vulns  []vulnerability
}

// Handle runs one scan job; it matches worker.Handler. Args are
// {"digest": ..., "kind": ...}.
func (s *Scanner) Handle(ctx context.Context, job queue.Job) (json.RawMessage, error) {
	var args scanjob.Args
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return nil, fmt.Errorf("decoding scan job %d args: %w", job.ID, err)
	}
	digest, err := model.ParseDigest(args.Digest)
	if err != nil {
		return nil, fmt.Errorf("scan job %d: %w", job.ID, err)
	}
	kind := model.SubjectKind(args.Kind)
	if kind == model.SubjectKindFile {
		s.logger.InfoContext(ctx, "scan skipped: unsupported kind", "job", job.ID, "kind", args.Kind)
		return json.RawMessage(`{"skipped":"unsupported kind"}`), nil
	}

	if s.opts.TempDir != "" {
		// #nosec G301,G703 -- 0700, under the cache directory the operator configured.
		if err := os.MkdirAll(s.opts.TempDir, 0o700); err != nil {
			return nil, fmt.Errorf("creating staging parent directory: %w", err)
		}
	}
	root, err := os.MkdirTemp(s.opts.TempDir, "sindri-scan-*")
	if err != nil {
		return nil, fmt.Errorf("creating staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	// Trivy is given absolute paths whatever TempDir looked like.
	if root, err = filepath.Abs(root); err != nil {
		return nil, fmt.Errorf("resolving staging directory: %w", err)
	}

	if args.Repository == "" {
		// Enqueued before jobs named their repository: there is nothing to
		// check placement against, so fail closed.
		return s.skippedNotInRepository(ctx, job, digest)
	}

	targets, err := s.stage(ctx, args.Repository, root, kind, digest)
	var notIn *notInRepositoryError
	if errors.As(err, &notIn) {
		return s.skippedNotInRepository(ctx, job, notIn.digest)
	}
	var dup *duplicatePlatformError
	if errors.As(err, &dup) {
		s.logger.WarnContext(ctx, "scan skipped: duplicate platform", "job", job.ID, "platform", dup.platform)
		return skipResult(struct {
			Skipped  string `json:"skipped"`
			Platform string `json:"platform"`
		}{"duplicate platform", dup.platform})
	}
	var unsupported *unsupportedChildError
	if errors.As(err, &unsupported) {
		s.logger.WarnContext(ctx, "scan skipped: unsupported index child", "job", job.ID, "digest", unsupported.digest.String(), "media_type", unsupported.mediaType)
		return skipResult(struct {
			Skipped   string `json:"skipped"`
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
		}{"unsupported child", unsupported.digest.String(), unsupported.mediaType})
	}
	if errors.Is(err, errTooManyPlatforms) || errors.Is(err, errNoPlatforms) {
		// Partial or empty scans are never attached, so a severity gate
		// refuses the subject (fail closed). Retrying would not change it.
		reason := "no scannable platforms"
		if errors.Is(err, errTooManyPlatforms) {
			reason = "too many platforms"
		}
		s.logger.WarnContext(ctx, "scan skipped", "job", job.ID, "subject", digest.String(), "reason", reason)
		return json.RawMessage(`{"skipped":"` + reason + `"}`), nil
	}
	if err != nil {
		return nil, fmt.Errorf("staging %s: %w", digest, err)
	}

	// Scan every target before attaching anything, so a failure leaves no
	// scan without its sbom and, for an index, usually nothing at all. If an
	// attach fails midway, a retry attaches the earlier platforms again;
	// duplicates are accepted because the gate reads the newest scan per
	// platform.
	var done []scanned
	for i, t := range targets {
		outDir := filepath.Join(root, fmt.Sprintf("out-%d", i))
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			return nil, fmt.Errorf("creating output directory: %w", err)
		}
		report, sbom, err := s.scanTarget(ctx, t, outDir)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", digest, err)
		}
		counts, vulns, err := readCounts(report)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", digest, err)
		}
		done = append(done, scanned{t: t, report: report, sbom: sbom, counts: counts, vulns: vulns})
	}

	version := s.versionInfo(ctx, root)
	results := make([]targetResult, 0, len(done))
	for _, d := range done {
		meta, err := json.Marshal(evidenceMetadata{
			Producer: "sindri", Scanner: "trivy",
			ScannerVersion: version.Version, DBUpdatedAt: version.DB.UpdatedAt,
			SeverityCounts: d.counts, Vulnerabilities: d.vulns, Coverage: d.t.coverage, Platform: d.t.platform,
		})
		if err != nil {
			return nil, fmt.Errorf("encoding evidence metadata: %w", err)
		}
		sbom, err := s.attachFile(ctx, digest, model.EvidenceSBOM, MediaTypeSBOM, d.sbom, meta)
		if err != nil {
			return nil, err
		}
		scan, err := s.attachFile(ctx, digest, model.EvidenceScan, MediaTypeScan, d.report, meta)
		if err != nil {
			return nil, err
		}
		results = append(results, targetResult{Platform: d.t.platform, SBOMID: sbom.ID, ScanID: scan.ID, SeverityCounts: d.counts})
		s.logger.InfoContext(ctx, "scan attached", "job", job.ID, "subject", digest.String(), "platform", d.t.platform)
	}
	out, err := json.Marshal(struct {
		Targets []targetResult `json:"targets"`
	}{results})
	if err != nil {
		return nil, fmt.Errorf("encoding scan result: %w", err)
	}
	return out, nil
}

// skippedNotInRepository ends a job that would read a blob the publishing
// repository does not hold. Nothing is attached, so a severity gate refuses
// the subject; retrying would not change it.
func (s *Scanner) skippedNotInRepository(ctx context.Context, job queue.Job, digest model.Digest) (json.RawMessage, error) {
	s.logger.WarnContext(ctx, "scan skipped: blob not in repository", "job", job.ID, "digest", digest.String())
	return skipResult(struct {
		Skipped string `json:"skipped"`
		Digest  string `json:"digest"`
	}{"blob not in repository", digest.String()})
}

func skipResult[T any](v T) (json.RawMessage, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding scan result: %w", err)
	}
	return out, nil
}

func readCounts(path string) (map[string]int, []vulnerability, error) {
	// #nosec G304 -- path is a file this process created inside its own staging directory.
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening report: %w", err)
	}
	defer func() { _ = f.Close() }()
	return countSeverities(f)
}

func (s *Scanner) attachFile(ctx context.Context, subject model.Digest, typ model.EvidenceType, mediaType, path string, meta json.RawMessage) (evidence.Evidence, error) {
	// #nosec G304 -- path is a file this process created inside its own staging directory.
	f, err := os.Open(path)
	if err != nil {
		return evidence.Evidence{}, fmt.Errorf("opening %s: %w", typ, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return evidence.Evidence{}, fmt.Errorf("statting %s: %w", typ, err)
	}
	ev, err := s.ev.Attach(ctx, evidence.AttachParams{
		Subject: subject, Type: typ, MediaType: mediaType, Body: f, Size: info.Size(),
		Actor: scanjob.Actor, Metadata: meta,
	})
	if err != nil {
		return evidence.Evidence{}, fmt.Errorf("attaching %s to %s: %w", typ, subject, err)
	}
	return ev, nil
}
