-- name: EnqueueJob :one
-- Inserts a job unless one with the same dedupe key is already active, in
-- which case no row comes back and the caller reads the active one.
INSERT INTO jobs (kind, args, dedupe_key, max_attempts, run_after, created_by)
VALUES (
    sqlc.arg('kind'), sqlc.arg('args'), sqlc.narg('dedupe_key'), sqlc.arg('max_attempts'),
    now() + (sqlc.arg('delay_seconds')::bigint * interval '1 second'),
    sqlc.arg('created_by')
)
ON CONFLICT (dedupe_key) WHERE state IN ('queued', 'running') DO NOTHING
RETURNING *;

-- name: GetActiveJobByDedupeKey :one
SELECT * FROM jobs
WHERE dedupe_key = $1 AND state IN ('queued', 'running');

-- name: GetJob :one
SELECT * FROM jobs
WHERE id = $1;

-- name: ClaimJob :one
-- Claims the oldest runnable job of a kind this worker knows. SKIP LOCKED lets
-- any number of workers claim concurrently without ever sharing a job.
UPDATE jobs
SET state = 'running',
    attempts = attempts + 1,
    lease_owner = sqlc.arg('owner'),
    lease_expires_at = now() + (sqlc.arg('lease_seconds')::bigint * interval '1 second'),
    started_at = now(),
    last_error = NULL
WHERE id = (
    SELECT j.id FROM jobs j
    WHERE j.state = 'queued'
      AND j.run_after <= now()
      AND j.kind = ANY (sqlc.arg('kinds')::text[])
    ORDER BY j.run_after, j.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: ExtendJobLease :execrows
-- A heartbeat. No row means the lease was lost, and the handler must stop.
UPDATE jobs
SET lease_expires_at = now() + (sqlc.arg('lease_seconds')::bigint * interval '1 second')
WHERE id = sqlc.arg('id') AND state = 'running' AND lease_owner = sqlc.arg('owner');

-- name: CompleteJob :execrows
UPDATE jobs
SET state = 'succeeded',
    result = sqlc.arg('result'),
    finished_at = now(),
    lease_owner = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg('id') AND state = 'running' AND lease_owner = sqlc.arg('owner');

-- name: FailJob :execrows
-- Requeues with exponential backoff, capped at an hour, until the attempts
-- run out; then the job stays failed with its last error.
UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
    last_error = sqlc.arg('last_error'),
    run_after = now() + LEAST(
        interval '1 hour',
        (sqlc.arg('backoff_seconds')::bigint * interval '1 second') * power(2, GREATEST(attempts - 1, 0))
    ),
    finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
    lease_owner = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg('id') AND state = 'running' AND lease_owner = sqlc.arg('owner');

-- name: ReleaseJob :execrows
-- Gives a job back without counting the attempt, for a worker that is
-- shutting down mid-job.
UPDATE jobs
SET state = 'queued',
    attempts = GREATEST(attempts - 1, 0),
    run_after = now(),
    lease_owner = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg('id') AND state = 'running' AND lease_owner = sqlc.arg('owner');

-- name: ReapExpiredLeases :execrows
-- A worker that died mid-job never releases it. Once the lease has run out
-- the attempt counts, and the job is retried or, out of attempts, failed.
UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
    last_error = 'lease expired',
    run_after = now(),
    finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
    lease_owner = NULL,
    lease_expires_at = NULL
WHERE state = 'running' AND lease_expires_at < now();

-- name: JobDue :one
-- Whether a scheduler should enqueue another job of this kind: nothing of the
-- kind is active, and nothing succeeded within the interval.
SELECT (
    NOT EXISTS (
        SELECT 1 FROM jobs
        WHERE jobs.dedupe_key = sqlc.arg('job_key')::text AND jobs.state IN ('queued', 'running')
    )
    AND NOT EXISTS (
        SELECT 1 FROM jobs
        WHERE jobs.kind = sqlc.arg('job_kind')::text AND jobs.state = 'succeeded'
          AND jobs.finished_at > now() - (sqlc.arg('interval_seconds')::bigint * interval '1 second')
    )
)::bool AS due;

-- name: DeleteFinishedJobs :execrows
DELETE FROM jobs
WHERE state IN ('succeeded', 'failed')
  AND finished_at < now() - (sqlc.arg('older_than_seconds')::bigint * interval '1 second');
