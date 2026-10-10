// Package worker runs background jobs from the queue table (ADR 20).
//
// A worker knows a set of job kinds and a handler for each. It claims one job
// at a time, keeps the lease alive while the handler runs, and records the
// outcome. It also enqueues recurring kinds on their schedules, and any
// number of workers may do so at once: the queue's dedupe key admits one.
//
// Nothing here assumes the worker shares a process with the API. The worker
// starts from a database and a set of handlers; the post-v1 --worker-only
// mode is the same call without the HTTP server beside it.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/spj/sindri/internal/queue"
)

// Handler does one job. The result is stored on the job for whoever enqueued
// it to read. The context is cancelled when the worker stops or the lease is
// lost, and a handler must return promptly when it is.
type Handler func(ctx context.Context, job queue.Job) (json.RawMessage, error)

// Schedule enqueues Kind every Interval. The kind is its own dedupe key, so a
// scheduled run and one requested by hand never overlap.
type Schedule struct {
	Kind     string
	Interval time.Duration
	Args     json.RawMessage
}

// Options configures Run.
type Options struct {
	Queue    *queue.Queue
	Handlers map[string]Handler
	// Owner identifies this worker on the leases it holds. It must be unique
	// among running workers.
	Owner string
	// Poll is how long the worker sleeps when there is nothing to claim.
	Poll time.Duration
	// Lease is how long a claim lasts without a heartbeat. The worker renews
	// it every third of its length.
	Lease time.Duration
	// Backoff is the first retry delay of a failed job; each later attempt
	// doubles it.
	Backoff   time.Duration
	Schedules []Schedule
	Logger    *slog.Logger
}

// Defaults for Options left zero.
const (
	DefaultPoll    = 5 * time.Second
	DefaultLease   = 2 * time.Minute
	DefaultBackoff = 30 * time.Second
)

// releaseTimeout bounds the hand-back of a job when the worker stops, which
// runs after the worker's own context is already cancelled.
const releaseTimeout = 5 * time.Second

// Run claims and runs jobs until ctx is cancelled. A job in progress when ctx
// is cancelled is handed back to the queue without counting the attempt.
func Run(ctx context.Context, opts Options) error {
	if opts.Queue == nil {
		return errors.New("worker: a queue is required")
	}
	if opts.Owner == "" {
		return errors.New("worker: an owner is required")
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.Backoff <= 0 {
		opts.Backoff = DefaultBackoff
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	for _, s := range opts.Schedules {
		if _, ok := opts.Handlers[s.Kind]; !ok {
			return fmt.Errorf("worker: schedule for %s, which has no handler", s.Kind)
		}
	}

	w := &worker{opts: opts, kinds: slices.Sorted(maps.Keys(opts.Handlers))}
	w.opts.Logger.InfoContext(ctx, "worker started", "owner", opts.Owner, "kinds", w.kinds)
	defer w.opts.Logger.InfoContext(context.WithoutCancel(ctx), "worker stopped", "owner", opts.Owner)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		w.tick(ctx)
		timer.Reset(opts.Poll)
	}
}

type worker struct {
	opts  Options
	kinds []string
}

// tick does one round: reap dead workers' leases, enqueue what is due, then
// run jobs until none is claimable.
func (w *worker) tick(ctx context.Context) {
	log := w.opts.Logger
	if n, err := w.opts.Queue.ReapExpired(ctx); err != nil {
		w.logError(ctx, "reaping expired leases", err)
	} else if n > 0 {
		log.WarnContext(ctx, "requeued jobs whose worker stopped holding the lease", "jobs", n)
	}
	w.schedule(ctx)

	for ctx.Err() == nil {
		job, ok, err := w.opts.Queue.Claim(ctx, w.opts.Owner, w.kinds, w.opts.Lease)
		if err != nil {
			w.logError(ctx, "claiming a job", err)
			return
		}
		if !ok {
			return
		}
		w.run(ctx, job)
	}
}

func (w *worker) schedule(ctx context.Context) {
	for _, s := range w.opts.Schedules {
		if s.Interval <= 0 {
			continue
		}
		due, err := w.opts.Queue.Due(ctx, s.Kind, s.Kind, s.Interval)
		if err != nil {
			w.logError(ctx, "checking a schedule", err)
			continue
		}
		if !due {
			continue
		}
		job, created, err := w.opts.Queue.Enqueue(ctx, queue.EnqueueParams{
			Kind:      s.Kind,
			Args:      s.Args,
			DedupeKey: s.Kind,
			CreatedBy: "schedule",
		})
		if err != nil {
			w.logError(ctx, "enqueueing a scheduled job", err)
			continue
		}
		if created {
			w.opts.Logger.InfoContext(ctx, "scheduled job enqueued", "kind", s.Kind, "job", job.ID)
		}
	}
}

// run executes one claimed job under a heartbeat and records its outcome.
func (w *worker) run(ctx context.Context, job queue.Job) {
	log := w.opts.Logger.With("job", job.ID, "kind", job.Kind, "attempt", job.Attempts)
	log.InfoContext(ctx, "job started")

	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		w.heartbeat(jobCtx, cancel, job.ID)
	}()

	result, err := w.call(jobCtx, job)
	cancel(nil)
	<-heartbeatDone

	// Record the outcome on a context that survives the worker stopping, so
	// a job is never left running until its lease expires.
	recordCtx, done := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer done()

	switch {
	case errors.Is(context.Cause(jobCtx), queue.ErrLeaseLost):
		log.WarnContext(ctx, "job abandoned: the lease was lost")
	case ctx.Err() != nil:
		if rerr := w.opts.Queue.Release(recordCtx, job.ID, w.opts.Owner); rerr != nil {
			w.logError(recordCtx, "handing back a job on shutdown", rerr)
			return
		}
		log.InfoContext(recordCtx, "job handed back on shutdown")
	case err != nil:
		if ferr := w.opts.Queue.Fail(recordCtx, job.ID, w.opts.Owner, err.Error(), w.opts.Backoff); ferr != nil {
			w.logError(recordCtx, "recording a failed job", ferr)
			return
		}
		log.WarnContext(ctx, "job failed", "error", err.Error())
	default:
		if cerr := w.opts.Queue.Complete(recordCtx, job.ID, w.opts.Owner, result); cerr != nil {
			w.logError(recordCtx, "recording a finished job", cerr)
			return
		}
		log.InfoContext(ctx, "job succeeded")
	}
}

// call runs the handler, turning a panic into a failed attempt: one bad job
// must not take the worker, and the server it may share a process with, down.
func (w *worker) call(ctx context.Context, job queue.Job) (result json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	h, ok := w.opts.Handlers[job.Kind]
	if !ok {
		return nil, fmt.Errorf("no handler for %s", job.Kind)
	}
	return h(ctx, job)
}

// heartbeat renews the lease until ctx ends. A renewal the queue refuses means
// another worker owns the job now, so the handler is cancelled.
func (w *worker) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, id int64) {
	t := time.NewTicker(max(w.opts.Lease/3, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := w.opts.Queue.Extend(ctx, id, w.opts.Owner, w.opts.Lease)
		switch {
		case err == nil:
		case errors.Is(err, queue.ErrLeaseLost):
			cancel(queue.ErrLeaseLost)
			return
		case ctx.Err() != nil:
			return
		default:
			// A transient database error: keep the job running and try again
			// on the next beat. If the lease does run out, the next renewal
			// reports it lost.
			w.logError(ctx, "renewing a lease", err)
		}
	}
}

func (w *worker) logError(ctx context.Context, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	w.opts.Logger.ErrorContext(ctx, what, "error", err.Error())
}
