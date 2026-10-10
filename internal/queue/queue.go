// Package queue is the jobs table: the one way background work is handed to
// a worker (ARCHITECTURE.md section 9, ADR 20).
//
// The API and the worker share nothing but this table. A job is enqueued by
// whoever wants it done, claimed by whichever worker gets there first, and
// held under a lease that a dead worker cannot keep. That is what lets the
// worker move into its own process later without a line changing here.
//
// Every time the queue compares or records comes from the database's clock,
// so workers on different hosts agree on when a lease ran out.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/spj/sindri/internal/db"
)

var (
	// ErrNotFound means no job has that id.
	ErrNotFound = errors.New("job not found")

	// ErrLeaseLost means the caller no longer holds the job: its lease ran
	// out and the job was reaped, or another worker has it now. The caller
	// must stop working on it.
	ErrLeaseLost = errors.New("job lease lost")
)

// State is where a job is in its life.
type State string

// The job states.
const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

// DefaultMaxAttempts is how often a job runs before it stays failed.
const DefaultMaxAttempts = 5

// Job is one row of the queue.
type Job struct {
	ID          int64
	Kind        string
	Args        json.RawMessage
	State       State
	DedupeKey   string
	Attempts    int
	MaxAttempts int
	CreatedBy   string
	CreatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	LastError   string
	Result      json.RawMessage
}

// Done reports whether the job has finished, successfully or not.
func (j Job) Done() bool { return j.State == StateSucceeded || j.State == StateFailed }

// Queue reads and writes the jobs table.
type Queue struct {
	q *db.Queries
}

// New returns a Queue on database.
func New(database *db.DB) *Queue {
	return &Queue{q: database.Queries()}
}

// EnqueueParams describes a job to add.
type EnqueueParams struct {
	Kind string
	Args json.RawMessage
	// DedupeKey, when set, allows at most one queued or running job with the
	// key. Enqueueing a key that is active returns the active job instead.
	DedupeKey   string
	MaxAttempts int
	Delay       time.Duration
	CreatedBy   string
}

// Enqueue adds a job, or returns the active job with the same dedupe key.
// created reports which of the two happened.
func (q *Queue) Enqueue(ctx context.Context, p EnqueueParams) (Job, bool, error) {
	if p.Kind == "" {
		return Job{}, false, errors.New("enqueueing a job: kind is required")
	}
	if p.CreatedBy == "" {
		return Job{}, false, errors.New("enqueueing a job: created_by is required")
	}
	args := p.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	key := pgtype.Text{String: p.DedupeKey, Valid: p.DedupeKey != ""}

	// The insert and the read of the active job are two statements, so the
	// active job can finish between them. Each round either inserts or finds
	// one; a few rounds cover any realistic interleaving.
	for range 3 {
		row, err := q.q.EnqueueJob(ctx, db.EnqueueJobParams{
			Kind:         p.Kind,
			Args:         args,
			DedupeKey:    key,
			MaxAttempts:  clampInt32(maxAttempts),
			DelaySeconds: seconds(p.Delay),
			CreatedBy:    p.CreatedBy,
		})
		if err == nil {
			return jobFromRow(row), true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Job{}, false, fmt.Errorf("enqueueing %s: %w", p.Kind, err)
		}
		active, err := q.q.GetActiveJobByDedupeKey(ctx, key)
		if err == nil {
			return jobFromRow(active), false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Job{}, false, fmt.Errorf("reading the active %s job: %w", p.Kind, err)
		}
	}
	return Job{}, false, fmt.Errorf("enqueueing %s: the active job kept changing", p.Kind)
}

// Get returns a job by id.
func (q *Queue) Get(ctx context.Context, id int64) (Job, error) {
	row, err := q.q.GetJob(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, fmt.Errorf("reading job %d: %w", id, err)
	}
	return jobFromRow(row), nil
}

// Claim takes the oldest runnable job of one of kinds for owner, holding it
// for lease. ok is false when there is nothing to do.
func (q *Queue) Claim(ctx context.Context, owner string, kinds []string, lease time.Duration) (Job, bool, error) {
	row, err := q.q.ClaimJob(ctx, db.ClaimJobParams{
		Owner:        text(owner),
		LeaseSeconds: leaseSeconds(lease),
		Kinds:        kinds,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, false, nil
		}
		return Job{}, false, fmt.Errorf("claiming a job: %w", err)
	}
	return jobFromRow(row), true, nil
}

// Extend renews owner's lease on a running job.
func (q *Queue) Extend(ctx context.Context, id int64, owner string, lease time.Duration) error {
	n, err := q.q.ExtendJobLease(ctx, db.ExtendJobLeaseParams{
		LeaseSeconds: leaseSeconds(lease),
		ID:           id,
		Owner:        text(owner),
	})
	return leaseResult("extending the lease on", id, n, err)
}

// Complete marks a running job succeeded with result.
func (q *Queue) Complete(ctx context.Context, id int64, owner string, result json.RawMessage) error {
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	n, err := q.q.CompleteJob(ctx, db.CompleteJobParams{Result: result, ID: id, Owner: text(owner)})
	return leaseResult("completing", id, n, err)
}

// Fail records a failed attempt. The job is retried after backoff, doubled
// for each earlier attempt and capped at an hour, until its attempts run out.
func (q *Queue) Fail(ctx context.Context, id int64, owner, cause string, backoff time.Duration) error {
	n, err := q.q.FailJob(ctx, db.FailJobParams{
		LastError:      text(cause),
		BackoffSeconds: seconds(backoff),
		ID:             id,
		Owner:          text(owner),
	})
	return leaseResult("failing", id, n, err)
}

// Release hands a running job back without counting the attempt, for a
// worker that is stopping before the job finished.
func (q *Queue) Release(ctx context.Context, id int64, owner string) error {
	n, err := q.q.ReleaseJob(ctx, db.ReleaseJobParams{ID: id, Owner: text(owner)})
	return leaseResult("releasing", id, n, err)
}

// ReapExpired requeues, or fails once out of attempts, every running job
// whose lease has run out: the worker that held it is gone.
func (q *Queue) ReapExpired(ctx context.Context) (int64, error) {
	n, err := q.q.ReapExpiredLeases(ctx)
	if err != nil {
		return 0, fmt.Errorf("reaping expired leases: %w", err)
	}
	return n, nil
}

// Due reports whether a scheduler should enqueue kind again: no job with
// dedupeKey is active, and none of kind succeeded within interval.
func (q *Queue) Due(ctx context.Context, kind, dedupeKey string, interval time.Duration) (bool, error) {
	due, err := q.q.JobDue(ctx, db.JobDueParams{
		JobKey:          dedupeKey,
		JobKind:         kind,
		IntervalSeconds: seconds(interval),
	})
	if err != nil {
		return false, fmt.Errorf("checking whether %s is due: %w", kind, err)
	}
	return due, nil
}

// DeleteFinished removes jobs that finished more than olderThan ago.
func (q *Queue) DeleteFinished(ctx context.Context, olderThan time.Duration) (int64, error) {
	n, err := q.q.DeleteFinishedJobs(ctx, seconds(olderThan))
	if err != nil {
		return 0, fmt.Errorf("deleting finished jobs: %w", err)
	}
	return n, nil
}

func leaseResult(what string, id, n int64, err error) error {
	if err != nil {
		return fmt.Errorf("%s job %d: %w", what, id, err)
	}
	if n == 0 {
		return fmt.Errorf("%s job %d: %w", what, id, ErrLeaseLost)
	}
	return nil
}

func jobFromRow(r db.Job) Job {
	j := Job{
		ID:          r.ID,
		Kind:        r.Kind,
		Args:        json.RawMessage(r.Args),
		State:       State(r.State),
		DedupeKey:   r.DedupeKey.String,
		Attempts:    int(r.Attempts),
		MaxAttempts: int(r.MaxAttempts),
		CreatedBy:   r.CreatedBy,
		CreatedAt:   r.CreatedAt,
		LastError:   r.LastError.String,
		Result:      json.RawMessage(r.Result),
	}
	if r.StartedAt.Valid {
		t := r.StartedAt.Time
		j.StartedAt = &t
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		j.FinishedAt = &t
	}
	return j
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// seconds rounds d down to whole seconds, the queue's resolution.
func seconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(d / time.Second)
}

// leaseSeconds is never below one second: a zero lease would expire on claim.
func leaseSeconds(d time.Duration) int64 {
	return max(seconds(d), 1)
}

func clampInt32(n int) int32 {
	if n > 1<<31-1 {
		return 1<<31 - 1
	}
	return int32(n) //nolint:gosec // G115: bounded above by the check
}
