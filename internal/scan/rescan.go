package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
)

// rescanLister lists the subjects a re-scan covers (normally *db.Queries).
type rescanLister interface {
	ListApprovedSubjectsForRescan(ctx context.Context) ([]db.ListApprovedSubjectsForRescanRow, error)
}

// rescanQueue is the part of *queue.Queue the fan-out uses.
type rescanQueue interface {
	Enqueue(ctx context.Context, p queue.EnqueueParams) (queue.Job, bool, error)
}

// Rescanner is the handler of the rescan_approved job: it enqueues one
// ordinary scan job for every approved subject (ADR 31). It writes no
// evidence itself; the scan jobs do, and they re-check placement.
type Rescanner struct {
	subjects rescanLister
	queue    rescanQueue
	logger   *slog.Logger
}

// NewRescanner returns the fan-out handler.
func NewRescanner(subjects rescanLister, q rescanQueue, logger *slog.Logger) *Rescanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Rescanner{subjects: subjects, queue: q, logger: logger}
}

// RescanResult is the result recorded on a rescan_approved job.
type RescanResult struct {
	Subjects int `json:"subjects"`
	Enqueued int `json:"enqueued"`
}

// Handle runs one rescan_approved job; it matches worker.Handler.
func (r *Rescanner) Handle(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
	rows, err := r.subjects.ListApprovedSubjectsForRescan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing approved subjects: %w", err)
	}
	res := RescanResult{Subjects: len(rows)}
	for _, row := range rows {
		digest, err := model.ParseDigest(row.Digest)
		if err != nil {
			r.logger.WarnContext(ctx, "re-scan skipped a subject with a malformed digest", "digest", row.Digest, "error", err.Error())
			continue
		}
		args, err := json.Marshal(scanjob.Args{Digest: digest.String(), Kind: row.Kind, Repository: row.Repository})
		if err != nil {
			return nil, fmt.Errorf("encoding scan job args for %s: %w", digest, err)
		}
		_, created, err := r.queue.Enqueue(ctx, queue.EnqueueParams{
			Kind:      scanjob.JobKind,
			Args:      args,
			DedupeKey: scanjob.DedupeKey(digest.String()),
			CreatedBy: scanjob.Actor,
		})
		if err != nil {
			return nil, fmt.Errorf("enqueueing a scan of %s: %w", digest, err)
		}
		if created {
			res.Enqueued++
		}
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("encoding the re-scan result: %w", err)
	}
	return out, nil
}
