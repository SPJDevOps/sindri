// Package scan produces vulnerability scans and SBOMs for hosted artifacts
// (ADR 23, ADR 29). The job kind, actor and dedupe key are what the publish
// path and the worker must agree on, so that internal/registry can enqueue a
// scan job without importing the scanner: this package must never import
// registry, config, api, or a protocol adapter.
//
// The scanner stages a subject's bytes into a temporary directory, runs Trivy
// as an external executable (never as a library), and attaches a CycloneDX
// sbom and a Trivy JSON scan as evidence marked producer "sindri".
package scan

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/evidence"
)

// JobKind is the queue job kind of a scan.
const JobKind = "scan"

// Actor is the identity recorded as the creator of scan jobs and of the
// evidence the scanner attaches.
const Actor = "system:scanner"

// DedupeKey returns the queue dedupe key for scanning the subject with the
// given digest string.
func DedupeKey(digest string) string { return "scan:" + digest }

// Media types of the two documents one Trivy run produces.
const (
	// MediaTypeSBOM is the media type of the CycloneDX sbom evidence.
	MediaTypeSBOM = "application/vnd.cyclonedx+json"
	// MediaTypeScan is the media type of the Trivy JSON scan evidence.
	MediaTypeScan = "application/vnd.aquasec.trivy.report+json"
)

var (
	// ErrUnsupportedKind means the subject kind has no scannable contents.
	ErrUnsupportedKind = errors.New("unsupported subject kind")
	// ErrArchiveTooLarge means an archive unpacks past Options.MaxUnpackedBytes.
	ErrArchiveTooLarge = errors.New("archive exceeds the unpacked size limit")
	// ErrUnsafePath means an archive entry is absolute or escapes the
	// directory it unpacks into.
	ErrUnsafePath = errors.New("archive entry has an unsafe path")
)

// Options configures a Scanner. cmd/sindri maps the configuration file onto
// it; this package does not read configuration.
type Options struct {
	// Trivy is the executable path or name.
	Trivy string
	// DBRepository is an optional --db-repository mirror.
	DBRepository string
	// CacheDir is Trivy's --cache-dir.
	CacheDir string
	// Timeout bounds each target's scan. Zero means no limit.
	Timeout time.Duration
	// SkipDBUpdate passes --skip-db-update.
	SkipDBUpdate bool
	// MaxUnpackedBytes caps the bytes an archive may unpack to.
	MaxUnpackedBytes int64
	// TempDir is the parent of staging directories; empty means os.TempDir().
	TempDir string
}

// attacher is the part of *evidence.Service the scanner uses, so unit tests
// can capture attachments.
type attacher interface {
	Attach(ctx context.Context, p evidence.AttachParams) (evidence.Evidence, error)
}

// Scanner runs scan jobs.
type Scanner struct {
	store  *cas.Store
	ev     attacher
	opts   Options
	logger *slog.Logger

	// maxEntries caps archive entries; a field so tests can lower it.
	maxEntries int

	versionMu sync.Mutex
	// scannerVersion is the last Trivy version successfully read. The
	// database timestamp is never cached: it is read for every job.
	scannerVersion string
}

// New returns a Scanner reading subject bytes from store and attaching
// evidence through ev (normally *evidence.Service).
func New(store *cas.Store, ev attacher, opts Options, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{store: store, ev: ev, opts: opts, logger: logger, maxEntries: maxArchiveEntries}
}
