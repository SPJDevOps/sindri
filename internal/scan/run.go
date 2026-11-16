package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/notify"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
	"github.com/spj/sindri/internal/worker"
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
	// Scan rows only: the sbom row omits it. A pointer, so an empty list on a
	// scan is written as [] and not dropped.
	Vulnerabilities *[]vulnerability `json:"vulnerabilities,omitempty"`
	// NewVulnerabilities lists the vulnerability ids in this scan that the
	// previous Sindri scan of the same subject and platform did not have,
	// sorted (ADR 31). Absent on a first scan, and when the previous scan
	// listed no vulnerabilities; [] when there was one and nothing is new.
	// Scan rows only.
	NewVulnerabilities *[]string `json:"new_vulnerabilities,omitempty"`
	Coverage           string    `json:"coverage"`
	Platform           string    `json:"platform,omitempty"`
	// Platforms is every platform this job scans, sorted; set on index
	// children only. The gate requires a newest scan for each (ADR 30), so
	// a job that attached only some platforms cannot pass.
	Platforms []string `json:"platforms,omitempty"`
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

// errJobDeadline is the cancellation cause of a job that ran past
// Options.JobTimeout, which tells it apart from a lease loss or a shutdown
// cancelling the parent context.
var errJobDeadline = errors.New("scan job deadline exceeded")

// Handle runs one scan job; it matches worker.Handler. Args are
// {"digest": ..., "kind": ...}.
//
// Options.JobTimeout bounds staging and scanning. The deadline is checked once
// after the last target is scanned: if it has passed, nothing is attached and
// the job fails. Attaching then runs on the caller's context alone, so a
// deadline that expires mid-attach cannot leave a subject half attached; lease
// loss and shutdown still cancel it.
func (s *Scanner) Handle(ctx context.Context, job queue.Job) (json.RawMessage, error) {
	scanCtx := ctx
	if s.opts.JobTimeout > 0 {
		var cancel context.CancelFunc
		scanCtx, cancel = context.WithTimeoutCause(ctx, s.opts.JobTimeout, errJobDeadline)
		defer cancel()
	}
	res, attaching, err := s.handle(ctx, scanCtx, job)
	if err != nil && !attaching && errors.Is(context.Cause(scanCtx), errJobDeadline) {
		// A deadline expiry is transient (raise the timeout and it passes), so
		// whatever else failed alongside it must not fail the job for good:
		// keep the cause's message but drop its permanent marker.
		if errors.Is(err, worker.ErrPermanent) {
			err = errors.New(err.Error())
		}
		return nil, fmt.Errorf("scan job %d ran longer than scanning.jobTimeout (%s); nothing was attached, raise it for larger subjects: %w: %w",
			job.ID, s.opts.JobTimeout, context.DeadlineExceeded, err)
	}
	return res, err
}

// handle does the work. Staging and scanning run on scanCtx, everything after
// on ctx; attaching reports whether the attach phase was reached.
func (s *Scanner) handle(ctx, scanCtx context.Context, job queue.Job) (res json.RawMessage, attaching bool, err error) {
	res, err = s.handleScan(ctx, scanCtx, job, &attaching)
	return res, attaching, err
}

func (s *Scanner) handleScan(ctx, scanCtx context.Context, job queue.Job, attaching *bool) (json.RawMessage, error) {
	var args scanjob.Args
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return nil, worker.Permanent(fmt.Errorf("decoding scan job %d args: %w", job.ID, err))
	}
	digest, err := model.ParseDigest(args.Digest)
	if err != nil {
		return nil, worker.Permanent(fmt.Errorf("scan job %d: %w", job.ID, err))
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

	targets, err := s.stage(scanCtx, args.Repository, root, kind, digest)
	var notIn *notInRepositoryError
	if errors.As(err, &notIn) {
		return s.skippedNotInRepository(ctx, job, notIn.digest)
	}
	// Everything below fails the job for good: the subject itself is what is
	// wrong, so a retry would stage the same bytes and fail the same way.
	// Nothing is attached, so a severity gate refuses the subject (fail
	// closed), and last_error says why.
	var dup *duplicatePlatformError
	if errors.As(err, &dup) {
		return nil, worker.Permanent(fmt.Errorf("staging %s: %w", digest, err))
	}
	var unsupported *unsupportedChildError
	if errors.As(err, &unsupported) {
		return nil, worker.Permanent(fmt.Errorf("staging %s: %w", digest, err))
	}
	if errors.Is(err, errTooManyPlatforms) || errors.Is(err, errNoPlatforms) || errors.Is(err, ErrUnsafePath) {
		return nil, worker.Permanent(fmt.Errorf("staging %s: %w", digest, err))
	}
	if errors.Is(err, ErrArchiveTooLarge) {
		return nil, worker.Permanent(fmt.Errorf("staging %s: %w (scanning.maxUnpackedBytes (%d); raise it for larger archives)", digest, err, s.opts.MaxUnpackedBytes))
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
		report, sbom, err := s.scanTarget(scanCtx, t, outDir)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", digest, err)
		}
		counts, vulns, err := readCounts(report)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", digest, err)
		}
		done = append(done, scanned{t: t, report: report, sbom: sbom, counts: counts, vulns: vulns})
	}

	// The deadline is checked once, here, so a timed-out job attaches nothing.
	if err := scanCtx.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", digest, context.Cause(scanCtx))
	}
	*attaching = true

	// Read the previous scans once, before anything is attached, so this
	// job's own attachments never count as "previous".
	previous, err := s.ev.NewestSindriScans(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("reading previous scans of %s: %w", digest, err)
	}

	version := s.versionInfo(ctx, root)
	// An index lists every target, including a child with no platform, which
	// is keyed "" exactly as the gate's newest-scan-per-platform query keys
	// it. A single-manifest subject has no list.
	var platforms []string
	if kind == model.SubjectKindOCIIndex {
		for _, d := range done {
			platforms = append(platforms, d.t.platform)
		}
	}
	sort.Strings(platforms)
	results := make([]targetResult, 0, len(done))
	for _, d := range done {
		base := evidenceMetadata{
			Producer: "sindri", Scanner: "trivy",
			ScannerVersion: version.Version, DBUpdatedAt: version.DB.UpdatedAt,
			SeverityCounts: d.counts, Coverage: d.t.coverage, Platform: d.t.platform,
			Platforms: platforms,
		}
		sbomMeta, err := json.Marshal(base)
		if err != nil {
			return nil, fmt.Errorf("encoding evidence metadata: %w", err)
		}
		base.Vulnerabilities = &d.vulns
		base.NewVulnerabilities = newVulnerabilities(d.vulns, previous[d.t.platform])
		meta, err := json.Marshal(base)
		if err != nil {
			return nil, fmt.Errorf("encoding evidence metadata: %w", err)
		}
		sbom, err := s.attachFile(ctx, digest, model.EvidenceSBOM, MediaTypeSBOM, d.sbom, sbomMeta, nil)
		if err != nil {
			return nil, err
		}
		scan, err := s.attachFile(ctx, digest, model.EvidenceScan, MediaTypeScan, d.report, meta, s.findingHook(digest, d, base.NewVulnerabilities))
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

// findingHook returns the evidence.AttachParams.InTx hook that records
// finding.new in the scan evidence's own transaction (ADR 34), or nil when
// there is nothing to send. Either the evidence and its deliveries commit
// together or neither does, so an emit failure fails the attach and the job
// retries. The dedupe key names the subject, repository, platform, and the set of new ids,
// not the evidence row, so a retry that re-attaches sends nothing twice.
func (s *Scanner) findingHook(digest model.Digest, d scanned, newVulns *[]string) func(context.Context, *db.Queries, evidence.Evidence) error {
	if s.notifier == nil || newVulns == nil || len(*newVulns) == 0 {
		return nil
	}
	return func(ctx context.Context, q *db.Queries, ev evidence.Evidence) error {
		return s.emitFinding(ctx, q, digest, d, *newVulns, ev.ID)
	}
}

// emitFinding sends one finding.new per repository holding a live approval of
// the digest, so a webhook filtered to a repository hears about a digest that
// is approved there, whichever repository the scan read the bytes from.
func (s *Scanner) emitFinding(ctx context.Context, q *db.Queries, digest model.Digest, d scanned, newVulns []string, evidenceID string) error {
	approvals, err := q.ListApprovedRepositoryStages(ctx, digest.String())
	if err != nil {
		return fmt.Errorf("listing approvals of %s: %w", digest, err)
	}
	var repositories []string
	stages := map[string][]string{}
	for _, a := range approvals {
		if _, ok := stages[a.Repository]; !ok {
			repositories = append(repositories, a.Repository)
		}
		stages[a.Repository] = append(stages[a.Repository], a.Stage)
	}
	ids := slices.Clone(newVulns)
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	for _, repository := range repositories {
		_, err = s.notifier.EmitTx(ctx, q, notify.Event{
			Digest:     digest,
			Repository: repository,
			DedupeKey:  fmt.Sprintf("finding:%s:%s:%s:%s", digest, repository, d.t.platform, hex.EncodeToString(sum[:])),
			Data: notify.FindingData{
				NewVulnerabilities: ids,
				SeverityCounts:     d.counts,
				Platform:           d.t.platform,
				ApprovedStages:     stages[repository],
				EvidenceID:         evidenceID,
			},
		})
		if err != nil {
			return fmt.Errorf("emitting finding.new for %s in %s: %w", digest, repository, err)
		}
	}
	return nil
}

// newVulnerabilities returns the ids in current that the previous scan's
// metadata does not list, unique and sorted ascending. It compares ids only,
// not packages (ADR 31). A nil result means there is nothing to compare with:
// no previous scan, or one written before findings were listed (ADR 30).
func newVulnerabilities(current []vulnerability, previous json.RawMessage) *[]string {
	if len(previous) == 0 {
		return nil
	}
	var meta struct {
		Vulnerabilities *[]struct {
			ID string `json:"id"`
		} `json:"vulnerabilities"`
	}
	if err := json.Unmarshal(previous, &meta); err != nil || meta.Vulnerabilities == nil {
		return nil
	}
	had := make(map[string]struct{}, len(*meta.Vulnerabilities))
	for _, v := range *meta.Vulnerabilities {
		had[v.ID] = struct{}{}
	}
	added := make(map[string]struct{})
	for _, v := range current {
		if _, ok := had[v.ID]; !ok {
			added[v.ID] = struct{}{}
		}
	}
	out := make([]string, 0, len(added))
	for id := range added {
		out = append(out, id)
	}
	sort.Strings(out)
	return &out
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

func (s *Scanner) attachFile(ctx context.Context, subject model.Digest, typ model.EvidenceType, mediaType, path string, meta json.RawMessage, inTx func(context.Context, *db.Queries, evidence.Evidence) error) (evidence.Evidence, error) {
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
		Actor: scanjob.Actor, Metadata: meta, InTx: inTx,
	})
	if err != nil {
		return evidence.Evidence{}, fmt.Errorf("attaching %s to %s: %w", typ, subject, err)
	}
	return ev, nil
}
