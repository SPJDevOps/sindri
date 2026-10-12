//go:build integration

package queue_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/testsupport"
)

func newQueue(t *testing.T) (*queue.Queue, *db.DB) {
	t.Helper()
	database := testsupport.MigratedDB(t)
	return queue.New(database), database
}

// backdate moves a job's lease or run_after into the past. Every queue time
// comes from the database clock, so tests move rows rather than a fake clock.
func exec(t *testing.T, database *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := database.Pool().Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func enqueue(t *testing.T, q *queue.Queue, kind, key string) queue.Job {
	t.Helper()
	job, _, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: kind, DedupeKey: key, CreatedBy: "test"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return job
}

func TestQueue_ClaimIsExclusiveUnderConcurrency(t *testing.T) {
	q, _ := newQueue(t)
	const jobs, workers = 40, 8
	for i := range jobs {
		enqueue(t, q, "work", fmt.Sprintf("job-%d", i))
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]string{}
		wg      sync.WaitGroup
		dup     []int64
	)
	for w := range workers {
		owner := fmt.Sprintf("worker-%d", w)
		wg.Go(func() {
			for {
				job, ok, err := q.Claim(t.Context(), owner, []string{"work"}, time.Minute)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				if _, seen := claimed[job.ID]; seen {
					dup = append(dup, job.ID)
				}
				claimed[job.ID] = owner
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(dup) > 0 {
		t.Fatalf("jobs claimed twice: %v", dup)
	}
	if len(claimed) != jobs {
		t.Fatalf("claimed %d jobs, want %d", len(claimed), jobs)
	}
}

func TestQueue_UnknownKindIsNeverClaimed(t *testing.T) {
	q, _ := newQueue(t)
	enqueue(t, q, "from-a-newer-binary", "")

	if _, ok, err := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute); err != nil || ok {
		t.Fatalf("Claim = ok %v, err %v; want nothing claimable", ok, err)
	}
}

func TestQueue_SingletonEnqueueReturnsTheActiveJob(t *testing.T) {
	q, _ := newQueue(t)

	first, created, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "gc", DedupeKey: "gc", CreatedBy: "a"})
	if err != nil || !created {
		t.Fatalf("first Enqueue = created %v, err %v", created, err)
	}
	second, created, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "gc", DedupeKey: "gc", CreatedBy: "b"})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("second Enqueue = id %d created %v, want the active job %d", second.ID, created, first.ID)
	}

	// Still the same job once it is running.
	if _, ok, err := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute); err != nil || !ok {
		t.Fatalf("Claim: ok %v err %v", ok, err)
	}
	third, created, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "gc", DedupeKey: "gc", CreatedBy: "c"})
	if err != nil || created || third.ID != first.ID {
		t.Fatalf("Enqueue while running = id %d created %v err %v, want %d", third.ID, created, err, first.ID)
	}
}

func TestQueue_SingletonAllowsANewJobAfterTheLastFinished(t *testing.T) {
	q, _ := newQueue(t)
	first := enqueue(t, q, "gc", "gc")
	job, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if err := q.Complete(t.Context(), job.ID, "w", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	next, created, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "gc", DedupeKey: "gc", CreatedBy: "a"})
	if err != nil || !created || next.ID == first.ID {
		t.Fatalf("Enqueue after completion = id %d created %v err %v", next.ID, created, err)
	}
}

func TestQueue_CompleteRecordsTheResult(t *testing.T) {
	q, _ := newQueue(t)
	enqueue(t, q, "gc", "")
	job, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if err := q.Complete(t.Context(), job.ID, "w", json.RawMessage(`{"deleted":3}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != queue.StateSucceeded || string(got.Result) != `{"deleted": 3}` || got.FinishedAt == nil {
		t.Fatalf("job = %+v", got)
	}
}

func TestQueue_GetUnknownJobIsNotFound(t *testing.T) {
	q, _ := newQueue(t)
	if _, err := q.Get(t.Context(), 4242); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
}

func TestQueue_ExpiredLeaseIsReclaimedAndCountsAnAttempt(t *testing.T) {
	q, database := newQueue(t)
	enqueue(t, q, "gc", "")
	job, _, _ := q.Claim(t.Context(), "dead-worker", []string{"gc"}, time.Minute)

	// A second worker cannot take a job whose lease is live.
	if _, ok, _ := q.Claim(t.Context(), "w2", []string{"gc"}, time.Minute); ok {
		t.Fatal("a job with a live lease was claimed again")
	}

	exec(t, database, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, job.ID)
	n, err := q.ReapExpired(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("ReapExpired = %d, %v; want 1", n, err)
	}

	again, ok, err := q.Claim(t.Context(), "w2", []string{"gc"}, time.Minute)
	if err != nil || !ok || again.ID != job.ID {
		t.Fatalf("reclaim = %+v ok %v err %v", again, ok, err)
	}
	if again.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2: the expired lease counts", again.Attempts)
	}
	// The dead worker comes back and tries to finish: refused.
	if err := q.Complete(t.Context(), job.ID, "dead-worker", nil); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("Complete by the previous owner = %v, want ErrLeaseLost", err)
	}
}

func TestQueue_CompleteAfterLeaseLossIsRejected(t *testing.T) {
	q, database := newQueue(t)
	enqueue(t, q, "gc", "")
	job, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	exec(t, database, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, job.ID)
	if _, err := q.ReapExpired(t.Context()); err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}

	for name, call := range map[string]func() error{
		"Complete": func() error { return q.Complete(t.Context(), job.ID, "w", nil) },
		"Fail":     func() error { return q.Fail(t.Context(), job.ID, "w", "boom", time.Second) },
		"Extend":   func() error { return q.Extend(t.Context(), job.ID, "w", time.Minute) },
		"Release":  func() error { return q.Release(t.Context(), job.ID, "w") },
	} {
		if err := call(); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s after losing the lease = %v, want ErrLeaseLost", name, err)
		}
	}
}

func TestQueue_FailedJobRetriesWithBackoffThenFails(t *testing.T) {
	q, database := newQueue(t)
	if _, _, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "gc", MaxAttempts: 2, CreatedBy: "t"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	job, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if err := q.Fail(t.Context(), job.ID, "w", "first failure", time.Hour); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ := q.Get(t.Context(), job.ID)
	if got.State != queue.StateQueued || got.LastError != "first failure" {
		t.Fatalf("after one failure = %+v, want queued with the error", got)
	}
	// The backoff holds it back.
	if _, ok, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute); ok {
		t.Fatal("a job inside its backoff was claimed")
	}

	exec(t, database, `UPDATE jobs SET run_after = now() WHERE id = $1`, job.ID)
	job, ok, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if !ok {
		t.Fatal("a job past its backoff was not claimed")
	}
	if err := q.Fail(t.Context(), job.ID, "w", "second failure", time.Second); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ = q.Get(t.Context(), job.ID)
	if got.State != queue.StateFailed || got.LastError != "second failure" || got.FinishedAt == nil {
		t.Fatalf("after max attempts = %+v, want failed", got)
	}
}

func TestQueue_ReleaseRequeuesWithoutCountingTheAttempt(t *testing.T) {
	q, _ := newQueue(t)
	enqueue(t, q, "gc", "")
	job, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if err := q.Release(t.Context(), job.ID, "w"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, ok, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if !ok || again.ID != job.ID || again.Attempts != 1 {
		t.Fatalf("after release = %+v ok %v, want the same job on attempt 1", again, ok)
	}
}

func TestQueue_DueOncePerInterval(t *testing.T) {
	q, database := newQueue(t)
	due := func() bool {
		t.Helper()
		ok, err := q.Due(t.Context(), "gc", "gc", time.Hour)
		if err != nil {
			t.Fatalf("Due: %v", err)
		}
		return ok
	}

	if !due() {
		t.Fatal("a kind that never ran is not due")
	}
	job := enqueue(t, q, "gc", "gc")
	if due() {
		t.Fatal("a kind with an active job is due")
	}
	claimed, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	if err := q.Complete(t.Context(), claimed.ID, "w", nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if due() {
		t.Fatal("a kind that just succeeded is due again")
	}
	exec(t, database, `UPDATE jobs SET finished_at = now() - interval '2 hours' WHERE id = $1`, job.ID)
	if !due() {
		t.Fatal("a kind whose last success is older than the interval is not due")
	}
}

func TestQueue_DeleteFinishedKeepsRecentAndActiveJobs(t *testing.T) {
	q, database := newQueue(t)
	old := enqueue(t, q, "gc", "")
	job, _, _ := q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	_ = q.Complete(t.Context(), job.ID, "w", nil)
	exec(t, database, `UPDATE jobs SET finished_at = now() - interval '40 days' WHERE id = $1`, old.ID)
	recent := enqueue(t, q, "gc", "")
	job, _, _ = q.Claim(t.Context(), "w", []string{"gc"}, time.Minute)
	_ = q.Complete(t.Context(), job.ID, "w", nil)
	active := enqueue(t, q, "gc", "")

	n, err := q.DeleteFinished(t.Context(), 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("DeleteFinished = %d, %v; want 1", n, err)
	}
	if _, err := q.Get(t.Context(), old.ID); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("old job: %v, want deleted", err)
	}
	for _, id := range []int64{recent.ID, active.ID} {
		if _, err := q.Get(t.Context(), id); err != nil {
			t.Fatalf("job %d: %v, want kept", id, err)
		}
	}
}
