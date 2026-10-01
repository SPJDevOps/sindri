-- name: UpsertSubject :exec
-- A subject is created on first publish and never rewritten: its digest names
-- the bytes, so neither size nor kind can change.
INSERT INTO subjects (digest, size, kind)
VALUES ($1, $2, $3)
ON CONFLICT (digest) DO NOTHING;

-- name: GetSubject :one
SELECT * FROM subjects
WHERE digest = $1;

-- name: AddSubjectOwner :exec
INSERT INTO subject_owners (digest, owner_ref)
VALUES ($1, $2)
ON CONFLICT (digest, owner_ref) DO NOTHING;

-- name: ListSubjectOwners :many
SELECT owner_ref FROM subject_owners
WHERE digest = $1
ORDER BY owner_ref;

-- name: RecordProvenance :exec
INSERT INTO provenance (digest, source_type, source_ref, build_ref)
VALUES ($1, $2, $3, $4)
ON CONFLICT ON CONSTRAINT provenance_claim_unique DO NOTHING;

-- name: ListProvenance :many
SELECT * FROM provenance
WHERE digest = $1
ORDER BY recorded_at, id;

-- name: InsertEvidence :one
INSERT INTO evidence (subject_digest, type, media_type, blob_digest, metadata, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListEvidence :many
SELECT * FROM evidence
WHERE subject_digest = $1
ORDER BY created_at, id;

-- name: GetEvidence :one
-- Scoped by subject: an evidence id alone never reaches bytes.
SELECT * FROM evidence
WHERE subject_digest = $1 AND id = $2;

-- name: ListEvidenceTypes :many
SELECT DISTINCT type FROM evidence
WHERE subject_digest = $1
ORDER BY type;

-- name: ListStages :many
SELECT * FROM lifecycle_stages
ORDER BY position;

-- name: GetStageByName :one
SELECT * FROM lifecycle_stages
WHERE name = $1;

-- name: UpsertStage :one
INSERT INTO lifecycle_stages (name, position)
VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE
SET position = EXCLUDED.position
RETURNING *;

-- name: DeleteStagesNotIn :execrows
DELETE FROM lifecycle_stages
WHERE NOT (name = ANY (sqlc.arg('keep')::text[]));

-- name: ListStageNamesWithHistoryNotIn :many
SELECT s.name FROM lifecycle_stages s
WHERE NOT (s.name = ANY (sqlc.arg('keep')::text[]))
  AND EXISTS (SELECT 1 FROM lifecycle_events e WHERE e.stage_id = s.id)
ORDER BY s.name;

-- name: DeleteGatesForStage :exec
DELETE FROM promotion_gates
WHERE stage_id = $1;

-- name: InsertGate :exec
INSERT INTO promotion_gates (stage_id, position, rule)
VALUES ($1, $2, $3);

-- name: ListGates :many
SELECT g.id, g.stage_id, g.rule, s.name AS stage_name
FROM promotion_gates g
JOIN lifecycle_stages s ON s.id = g.stage_id
ORDER BY s.position, g.position;

-- name: ListGatesForStage :many
SELECT * FROM promotion_gates
WHERE stage_id = $1
ORDER BY position;

-- name: AppendLifecycleEvent :one
-- The only write to the ledger. There is no UPDATE or DELETE query for this
-- table, and the database refuses one (invariant I3).
INSERT INTO lifecycle_events (subject_digest, repo_id, stage_id, action, actor, reason)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetCurrentState :one
SELECT * FROM lifecycle_current
WHERE subject_digest = $1 AND stage_id = $2 AND repo_id = $3;

-- name: ListCurrentApprovals :many
-- The admission decision: is exactly this digest approved for this stage in
-- any hosted repository? One indexed lookup on lifecycle_current.
SELECT c.repo_id, r.name AS repo_name, c.actor, c.reason, c.at
FROM lifecycle_current c
JOIN repositories r ON r.id = c.repo_id
WHERE c.subject_digest = sqlc.arg('subject_digest')
  AND c.stage_id = sqlc.arg('stage_id')
  AND c.action = 'approve'
ORDER BY r.name;

-- name: ListCurrentStates :many
SELECT c.action, c.actor, c.reason, c.at, r.name AS repo_name, s.name AS stage_name
FROM lifecycle_current c
JOIN repositories r ON r.id = c.repo_id
JOIN lifecycle_stages s ON s.id = c.stage_id
WHERE c.subject_digest = $1
ORDER BY s.position, r.name;

-- name: ListLifecycleEvents :many
SELECT e.id, e.action, e.actor, e.reason, e.at, r.name AS repo_name, s.name AS stage_name
FROM lifecycle_events e
JOIN repositories r ON r.id = e.repo_id
JOIN lifecycle_stages s ON s.id = e.stage_id
WHERE e.subject_digest = $1
ORDER BY e.at, e.id;
