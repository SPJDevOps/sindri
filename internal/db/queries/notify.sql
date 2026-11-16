-- name: InsertWebhookDelivery :one
-- Inserts a delivery unless this webhook already has one with the dedupe key,
-- in which case no row comes back: the event was already emitted.
INSERT INTO webhook_deliveries (webhook, event, subject_digest, repository, payload, dedupe_key)
VALUES (
    sqlc.arg('webhook'), sqlc.arg('event'), sqlc.arg('subject_digest'),
    sqlc.arg('repository'), sqlc.arg('payload'), sqlc.arg('dedupe_key')
)
ON CONFLICT (webhook, dedupe_key) DO NOTHING
RETURNING *;

-- name: SetWebhookDeliveryPayload :exec
-- The payload carries the delivery's own id, which exists only after the
-- insert.
UPDATE webhook_deliveries SET payload = sqlc.arg('payload') WHERE id = sqlc.arg('id');

-- name: SetWebhookDeliveryJob :exec
UPDATE webhook_deliveries SET job_id = sqlc.arg('job_id') WHERE id = sqlc.arg('id');

-- name: GetWebhookDelivery :one
-- job_run_after is when the delivery's job next runs. It is the next retry
-- only while state is pending; it is null once the job is gone.
SELECT d.*, j.run_after AS job_run_after
FROM webhook_deliveries d
LEFT JOIN jobs j ON j.id = d.job_id
WHERE d.id = $1;

-- name: ListWebhookDeliveries :many
SELECT d.*, j.run_after AS job_run_after
FROM webhook_deliveries d
LEFT JOIN jobs j ON j.id = d.job_id
WHERE (sqlc.narg('webhook')::text IS NULL OR d.webhook = sqlc.narg('webhook'))
  AND (sqlc.narg('state')::text IS NULL OR d.state = sqlc.narg('state'))
  AND (sqlc.narg('subject_digest')::text IS NULL OR d.subject_digest = sqlc.narg('subject_digest'))
ORDER BY d.created_at DESC, d.id DESC
LIMIT sqlc.arg('row_limit');

-- name: RecordWebhookAttempt :exec
UPDATE webhook_deliveries
SET attempts = attempts + 1,
    last_status = sqlc.narg('last_status'),
    last_error = sqlc.narg('last_error')
WHERE id = sqlc.arg('id');

-- name: MarkWebhookDelivered :exec
UPDATE webhook_deliveries
SET state = 'delivered', delivered_at = now(), finished_at = now(), last_error = NULL
WHERE id = $1;

-- name: MarkWebhookFailed :exec
UPDATE webhook_deliveries
SET state = 'failed', finished_at = now(), last_error = sqlc.arg('last_error')::text
WHERE id = sqlc.arg('id') AND state = 'pending';

-- name: FailOrphanedWebhookDeliveries :execrows
-- A pending delivery whose job failed, or no longer exists, will never get an
-- outcome: the worker died on the last attempt, or the job failed without
-- reaching the handler's bookkeeping. The age bound keeps a row whose job is
-- still being enqueued in another transaction out of reach.
UPDATE webhook_deliveries d
SET state = 'failed', finished_at = now(), last_error = sqlc.arg('last_error')::text
WHERE d.state = 'pending'
  AND d.created_at < now() - interval '1 minute'
  AND NOT EXISTS (
      SELECT 1 FROM jobs j WHERE j.id = d.job_id AND j.state <> 'failed'
  );

-- name: WebhookDeliveryStats :many
-- failed counts every failed delivery. failing is whether the newest finished
-- delivery failed, so a receiver that recovers, or a redelivery that succeeds,
-- clears it.
SELECT webhook,
       (count(*) FILTER (WHERE state = 'failed'))::bigint AS failed,
       COALESCE((array_agg(state ORDER BY finished_at DESC, id DESC)
                 FILTER (WHERE state IN ('delivered', 'failed')))[1] = 'failed', false)::boolean AS failing
FROM webhook_deliveries
GROUP BY webhook;

-- name: ListApprovedRepositoryStages :many
-- Every (repository, stage) at which this digest is currently approved. A
-- finding on a subject nobody approved is not sent, and one that is approved
-- is sent once per repository holding an approval (ADR 34).
SELECT r.name AS repository, s.name AS stage
FROM lifecycle_current c
JOIN lifecycle_stages s ON s.id = c.stage_id
JOIN repositories r ON r.id = c.repo_id
WHERE c.subject_digest = $1 AND c.action = 'approve'
ORDER BY r.name, s.position;
