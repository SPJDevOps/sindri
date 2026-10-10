//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/testsupport"
	"github.com/spj/sindri/internal/worker"
)

const poll = 20 * time.Millisecond

func setup(t *testing.T) (*queue.Queue, *db.DB) {
	t.Helper()
	database := testsupport.MigratedDB(t)
	return queue.New(database), database
}

// start runs a worker until the test ends or stop is called, and reports
// Run's return through the channel.
func start(t *testing.T, opts worker.Options) (stop func()) {
	t.Helper()
	if opts.Poll == 0 {
		opts.Poll = poll
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, opts) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("the worker did not stop within 10s of its context ending")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func jobState(t *testing.T, q *queue.Queue, id int64) queue.Job {
	t.Helper()
	job, err := q.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return job
}

func TestWorker_RunsAJobAndRecordsItsResult(t *testing.T) {
	q, _ := setup(t)
	job, _, _ := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "echo", Args: json.RawMessage(`{"n":7}`), CreatedBy: "t"})

	start(t, worker.Options{Queue: q, Owner: "w", Handlers: map[string]worker.Handler{
		"echo": func(_ context.Context, j queue.Job) (json.RawMessage, error) { return j.Args, nil },
	}})

	waitFor(t, "the job to succeed", func() bool { return jobState(t, q, job.ID).State == queue.StateSucceeded })
	if got := jobState(t, q, job.ID); string(got.Result) != `{"n": 7}` {
		t.Fatalf("result = %s", got.Result)
	}
}

func TestWorker_FailedHandlerRecordsTheErrorForRetry(t *testing.T) {
	q, _ := setup(t)
	job, _, _ := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "flaky", CreatedBy: "t"})

	start(t, worker.Options{Queue: q, Owner: "w", Backoff: time.Hour, Handlers: map[string]worker.Handler{
		"flaky": func(context.Context, queue.Job) (json.RawMessage, error) {
			return nil, errors.New("upstream timed out")
		},
	}})

	waitFor(t, "the failure to be recorded", func() bool { return jobState(t, q, job.ID).LastError != "" })
	got := jobState(t, q, job.ID)
	if got.State != queue.StateQueued || got.LastError != "upstream timed out" || got.Attempts != 1 {
		t.Fatalf("job = %+v, want queued for retry with the error", got)
	}
}

func TestWorker_PanickingHandlerFailsTheAttemptAndKeepsRunning(t *testing.T) {
	q, _ := setup(t)
	bad, _, _ := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "bad", MaxAttempts: 1, CreatedBy: "t"})
	good, _, _ := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "good", CreatedBy: "t"})

	start(t, worker.Options{Queue: q, Owner: "w", Handlers: map[string]worker.Handler{
		"bad":  func(context.Context, queue.Job) (json.RawMessage, error) { panic("boom") },
		"good": func(context.Context, queue.Job) (json.RawMessage, error) { return nil, nil },
	}})

	waitFor(t, "both jobs to finish", func() bool {
		return jobState(t, q, bad.ID).Done() && jobState(t, q, good.ID).Done()
	})
	if got := jobState(t, q, bad.ID); got.State != queue.StateFailed {
		t.Fatalf("panicking job = %+v, want failed", got)
	}
	if got := jobState(t, q, good.ID); got.State != queue.StateSucceeded {
		t.Fatalf("job after the panic = %+v, want succeeded", got)
	}
}

func TestWorker_StopsOnContextCancelAndRequeuesItsJob(t *testing.T) {
	q, _ := setup(t)
	job, _, _ := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "slow", CreatedBy: "t"})

	running := make(chan struct{})
	stop := start(t, worker.Options{Queue: q, Owner: "w", Handlers: map[string]worker.Handler{
		"slow": func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
			close(running)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}})

	<-running
	stop()

	got := jobState(t, q, job.ID)
	if got.State != queue.StateQueued || got.Attempts != 0 {
		t.Fatalf("job after shutdown = %+v, want queued with the attempt uncounted", got)
	}
}

func TestWorker_LostLeaseCancelsTheHandler(t *testing.T) {
	q, database := setup(t)
	job, _, _ := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "slow", CreatedBy: "t"})

	running := make(chan struct{})
	cancelled := make(chan error, 1)
	start(t, worker.Options{Queue: q, Owner: "w", Lease: time.Second, Handlers: map[string]worker.Handler{
		"slow": func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
			close(running)
			<-ctx.Done()
			cancelled <- context.Cause(ctx)
			return nil, ctx.Err()
		},
	}})

	<-running
	// Another worker took the job over after this one's lease was reaped.
	if _, err := database.Pool().Exec(t.Context(),
		`UPDATE jobs SET lease_owner = 'someone-else' WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("stealing the lease: %v", err)
	}

	select {
	case cause := <-cancelled:
		if !errors.Is(cause, queue.ErrLeaseLost) {
			t.Fatalf("handler cancelled with %v, want ErrLeaseLost", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler kept running after the lease was lost")
	}
	if got := jobState(t, q, job.ID); got.State != queue.StateRunning || got.Attempts != 1 {
		t.Fatalf("job = %+v: the worker that lost the lease must leave it to its new owner", got)
	}
}

func TestScheduler_EnqueuesOncePerIntervalAcrossTwoSchedulers(t *testing.T) {
	q, _ := setup(t)
	var runs atomic.Int64
	handlers := map[string]worker.Handler{
		"gc": func(context.Context, queue.Job) (json.RawMessage, error) {
			runs.Add(1)
			return nil, nil
		},
	}
	schedules := []worker.Schedule{{Kind: "gc", Interval: time.Hour}}
	start(t, worker.Options{Queue: q, Owner: "a", Handlers: handlers, Schedules: schedules})
	start(t, worker.Options{Queue: q, Owner: "b", Handlers: handlers, Schedules: schedules})

	waitFor(t, "the first scheduled run", func() bool { return runs.Load() >= 1 })
	// Many poll intervals later, still once.
	time.Sleep(50 * poll)
	if n := runs.Load(); n != 1 {
		t.Fatalf("scheduled job ran %d times within one interval across two schedulers, want 1", n)
	}
}

func TestRun_RejectsAScheduleWithoutAHandler(t *testing.T) {
	q, _ := setup(t)
	err := worker.Run(t.Context(), worker.Options{
		Queue: q, Owner: "w",
		Schedules: []worker.Schedule{{Kind: "gc", Interval: time.Hour}},
	})
	if err == nil {
		t.Fatal("Run accepted a schedule with no handler")
	}
}
