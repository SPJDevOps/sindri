// Package gc reclaims blobs nothing references (ADR 19).
//
// Collection has two phases. Mark proposes candidates: blobs no root reaches
// through manifest_children, last seen before the grace period. Sweep decides,
// one candidate at a time, under a lock on the blob's row: it re-checks every
// reference and the grace period, deletes the rows, deletes the bytes, and only
// then commits. Mark alone never deletes anything, which is what a dry run is.
//
// The safety argument does not rest on the mark snapshot or the grace period
// alone. Writers record their intent on the row before they write or rely on
// bytes (cas.Reserver), so a writer either commits its intent before the sweep
// locks the row, and the re-check sees it, or blocks on the lock until the
// sweep has committed, and then finds no bytes and writes them again.
package gc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

// JobKind is the queue kind for a collection, and its dedupe key: one
// collection at a time, whether scheduled or asked for with `sindri gc`.
const JobKind = "gc"

// ErrUnsafeWiring means the collector was given a CAS whose writes do not
// record intent. Running beside it could delete bytes a push is relying on.
var ErrUnsafeWiring = errors.New("garbage collection needs a CAS that records write intent")

// Defaults for Options left zero.
const (
	DefaultGrace       = 24 * time.Hour
	DefaultLockTimeout = 5 * time.Second
	// MaxPasses bounds the sweep. A manifest's children become collectable
	// only once the manifest itself has gone, so a chain index -> manifest ->
	// layer can need a pass per level when the order is unlucky.
	MaxPasses = 8
	markPage  = 1000
)

// Options configures a Collector.
type Options struct {
	Database *db.DB
	CAS      *cas.Store
	// Grace protects every blob seen more recently than this.
	Grace time.Duration
	// LockTimeout bounds how long a sweep step waits on a lock other than
	// the blob row, which it never waits for.
	LockTimeout time.Duration
	Logger      *slog.Logger
}

// Collector marks and sweeps.
type Collector struct {
	db          *db.DB
	cas         *cas.Store
	grace       time.Duration
	lockTimeout time.Duration
	log         *slog.Logger
}

// New returns a Collector, or ErrUnsafeWiring for a CAS without a reserver.
func New(opts Options) (*Collector, error) {
	if opts.Database == nil || opts.CAS == nil {
		return nil, errors.New("gc: a database and a CAS are required")
	}
	if !opts.CAS.HasReserver() {
		return nil, ErrUnsafeWiring
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultGrace
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = DefaultLockTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{
		db: opts.Database, cas: opts.CAS, grace: opts.Grace,
		lockTimeout: opts.LockTimeout, log: opts.Logger,
	}, nil
}

// Candidate is a blob the mark found unreachable.
type Candidate struct {
	Digest     model.Digest
	Size       int64
	LastSeenAt time.Time
	// IsParent reports that the blob is a manifest or index with children,
	// which the sweep takes first so that its children can follow.
	IsParent bool
}

// Result reports a collection.
type Result struct {
	Candidates        int   `json:"candidates"`
	Deleted           int   `json:"deleted"`
	BytesFreed        int64 `json:"bytes_freed"`
	SkippedRecent     int   `json:"skipped_recent"`
	SkippedReferenced int   `json:"skipped_referenced"`
	SkippedLocked     int   `json:"skipped_locked"`
	Errors            int   `json:"errors"`
	Passes            int   `json:"passes"`
}

// Mark returns every candidate, read in one snapshot.
func (c *Collector) Mark(ctx context.Context) ([]Candidate, error) {
	tx, err := c.db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("beginning the mark: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := c.db.Queries().WithTx(tx)

	var out []Candidate
	after := ""
	for {
		page, next, err := c.page(ctx, q, after, markPage)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// Candidates returns one page of the mark, ordered by digest, for a dry run
// listed over the API. next is empty on the last page. Each page is its own
// snapshot, which is fine for a report and is why Collect uses Mark instead.
func (c *Collector) Candidates(ctx context.Context, after string, limit int) ([]Candidate, string, error) {
	return c.page(ctx, c.db.Queries(), after, limit)
}

func (c *Collector) page(ctx context.Context, q *db.Queries, after string, limit int) ([]Candidate, string, error) {
	if limit <= 0 || limit > markPage {
		limit = markPage
	}
	rows, err := q.GCCandidates(ctx, db.GCCandidatesParams{
		GraceSeconds: c.graceSeconds(),
		AfterDigest:  pgtype.Text{String: after, Valid: after != ""},
		PageLimit:    int32(limit), //nolint:gosec // G115: bounded by markPage above
	})
	if err != nil {
		return nil, "", fmt.Errorf("marking: %w", err)
	}
	out := make([]Candidate, 0, len(rows))
	for _, r := range rows {
		d, err := model.ParseDigest(r.Digest)
		if err != nil {
			return nil, "", fmt.Errorf("marking: stored digest %q: %w", r.Digest, err)
		}
		out = append(out, Candidate{Digest: d, Size: r.Size, LastSeenAt: r.LastSeenAt, IsParent: r.IsParent})
	}
	next := ""
	if len(rows) == limit {
		next = rows[len(rows)-1].Digest
	}
	return out, next, nil
}

// Collect marks and sweeps.
func (c *Collector) Collect(ctx context.Context) (Result, error) {
	cands, err := c.Mark(ctx)
	if err != nil {
		return Result{}, err
	}
	return c.Sweep(ctx, cands)
}

type outcome int

const (
	deleted outcome = iota
	gone
	recent
	referenced
	locked
	failed
)

// Sweep deletes those of cands that are still unreachable and still outside
// the grace period. A candidate skipped because something still referenced it
// is tried again in a later pass if that pass deleted anything, since the
// reference may have been a parent the same sweep has since collected.
func (c *Collector) Sweep(ctx context.Context, cands []Candidate) (Result, error) {
	res := Result{Candidates: len(cands)}
	// Parents first: deleting a manifest is what frees its children.
	pending := slices.Clone(cands)
	slices.SortStableFunc(pending, func(a, b Candidate) int {
		return -cmp.Compare(boolInt(a.IsParent), boolInt(b.IsParent))
	})

	last := map[string]outcome{}
	for res.Passes < MaxPasses && len(pending) > 0 {
		res.Passes++
		progress := false
		var retry []Candidate
		for _, cand := range pending {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			out, err := c.sweepOne(ctx, cand)
			if err != nil {
				c.log.ErrorContext(ctx, "collecting a blob", "digest", cand.Digest.String(), "error", err.Error())
			}
			last[cand.Digest.String()] = out
			switch out {
			case deleted:
				progress = true
				res.Deleted++
				res.BytesFreed += cand.Size
			case referenced:
				retry = append(retry, cand)
			case gone, recent, locked, failed:
			}
		}
		if !progress {
			break
		}
		pending = retry
	}

	for _, out := range last {
		switch out {
		case recent:
			res.SkippedRecent++
		case referenced:
			res.SkippedReferenced++
		case locked:
			res.SkippedLocked++
		case failed:
			res.Errors++
		case deleted, gone:
		}
	}
	c.log.InfoContext(ctx, "garbage collection finished",
		"candidates", res.Candidates, "deleted", res.Deleted, "bytes_freed", res.BytesFreed,
		"skipped_recent", res.SkippedRecent, "skipped_referenced", res.SkippedReferenced,
		"skipped_locked", res.SkippedLocked, "errors", res.Errors, "passes", res.Passes)
	return res, nil
}

// sweepOne collects one candidate in one transaction.
//
// The order matters. Rows are deleted, then bytes, then the transaction
// commits, all while the blob row is locked. A writer that wants the blob
// blocks on the lock, and when it gets the row it is gone and so are the
// bytes, so it writes them again. Deleting the bytes after the commit instead
// would let a writer slip in between, find the bytes still there, skip
// writing them, and then lose them.
func (c *Collector) sweepOne(ctx context.Context, cand Candidate) (outcome, error) {
	digest := cand.Digest.String()
	tx, err := c.db.Pool().Begin(ctx)
	if err != nil {
		return failed, fmt.Errorf("beginning: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := c.db.Queries().WithTx(tx)

	if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)",
		fmt.Sprintf("%dms", c.lockTimeout.Milliseconds())); err != nil {
		return failed, fmt.Errorf("setting the lock timeout: %w", err)
	}

	row, err := q.LockBlobForGC(ctx, db.LockBlobForGCParams{GraceSeconds: c.graceSeconds(), Digest: digest})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return failed, fmt.Errorf("locking: %w", err)
		}
		exists, err := q.BlobExists(ctx, digest)
		if err != nil {
			return failed, fmt.Errorf("checking existence: %w", err)
		}
		if exists {
			return locked, nil
		}
		return gone, nil
	}
	if !row.Expired {
		return recent, nil
	}

	hasSubject, err := q.SubjectExists(ctx, digest)
	if err != nil {
		return failed, fmt.Errorf("checking the subject: %w", err)
	}
	if hasSubject {
		if _, err := q.LockSubjectForGC(ctx, digest); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return locked, nil
			}
			return failed, fmt.Errorf("locking the subject: %w", err)
		}
	}

	isReferenced, err := q.GCBlobReferenced(ctx, digest)
	if err != nil {
		return failed, fmt.Errorf("re-checking references: %w", err)
	}
	if isReferenced {
		return referenced, nil
	}

	for _, step := range []struct {
		what string
		del  func(context.Context, string) error
	}{
		{"manifest edges", q.GCDeleteManifestEdges},
		{"placements", q.GCDeletePlacements},
		{"owners", q.GCDeleteSubjectOwners},
		{"provenance", q.GCDeleteProvenance},
		{"the subject", q.GCDeleteSubject},
	} {
		if err := step.del(ctx, digest); err != nil {
			return failed, fmt.Errorf("deleting %s: %w", step.what, err)
		}
	}
	if _, err := q.GCDeleteBlob(ctx, digest); err != nil {
		return failed, fmt.Errorf("deleting the blob row: %w", err)
	}

	// Bytes last, under the lock, before the commit. An absent object is
	// success: a proxy placeholder never had bytes, and a retried sweep may
	// have deleted them already.
	if err := c.cas.Delete(ctx, cand.Digest); err != nil {
		return failed, fmt.Errorf("deleting bytes: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		// The bytes are gone and the row is not: a row without bytes is the
		// state a placeholder is always in, and the next run collects it.
		return failed, fmt.Errorf("committing: %w", err)
	}
	return deleted, nil
}

func (c *Collector) graceSeconds() int64 { return int64(c.grace / time.Second) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
