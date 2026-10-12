-- name: GCCandidates :many
-- The mark phase (ADR 19). A blob is live when a root reaches it, directly or
-- through manifest_children; everything else older than the grace period is a
-- candidate. The sweep re-checks each candidate under a row lock, so this
-- query only proposes: it never decides on its own that bytes may go.
--
-- Roots: tags, PyPI files, proxy cache hits, evidence documents and the
-- subjects they describe, and every subject with ledger history.
-- repository_blobs is deliberately not a root: a layer uploaded and never
-- referenced by a manifest is garbage.
WITH RECURSIVE roots (digest) AS (
    SELECT target_digest FROM tags
    UNION
    SELECT blob_digest FROM files
    UNION
    SELECT blob_digest FROM remote_cache WHERE blob_digest IS NOT NULL
    UNION
    SELECT blob_digest FROM evidence
    UNION
    SELECT subject_digest FROM evidence
    UNION
    SELECT subject_digest FROM lifecycle_events
),
live (digest) AS (
    SELECT digest FROM roots
    UNION
    SELECT mc.child_digest
    FROM manifest_children mc
    JOIN live l ON mc.parent_digest = l.digest
)
SELECT b.digest, b.size, b.last_seen_at,
       EXISTS (SELECT 1 FROM manifest_children p WHERE p.parent_digest = b.digest)::bool AS is_parent
FROM blobs b
WHERE b.last_seen_at < now() - (sqlc.arg('grace_seconds')::bigint * interval '1 second')
  AND NOT EXISTS (SELECT 1 FROM live l WHERE l.digest = b.digest)
  AND (sqlc.narg('after_digest')::text IS NULL OR b.digest > sqlc.narg('after_digest')::text)
ORDER BY b.digest
LIMIT sqlc.arg('page_limit');

-- name: LockBlobForGC :one
-- Takes the row lock the sweep holds while it deletes bytes. SKIP LOCKED: a
-- row some writer holds is in use by definition, so it is skipped, not waited
-- for. expired re-reads the grace period against the row as it is now.
SELECT digest, size,
       (last_seen_at < now() - (sqlc.arg('grace_seconds')::bigint * interval '1 second'))::bool AS expired
FROM blobs
WHERE digest = sqlc.arg('digest')
FOR UPDATE SKIP LOCKED;

-- name: BlobExists :one
SELECT EXISTS (SELECT 1 FROM blobs WHERE digest = $1)::bool AS exists;

-- name: SubjectExists :one
SELECT EXISTS (SELECT 1 FROM subjects WHERE digest = $1)::bool AS exists;

-- name: LockSubjectForGC :one
-- Blocks a concurrent promotion or evidence attach, whose foreign keys need
-- a share lock on this row, until the sweep has decided.
SELECT digest FROM subjects
WHERE digest = $1
FOR UPDATE SKIP LOCKED;

-- name: GCBlobReferenced :one
-- The sweep's re-check, run under the row locks. Every reference a live blob
-- can have, counted conservatively: an edge from a dead parent still counts,
-- and the next pass collects the child once the parent has gone.
SELECT (
    EXISTS (SELECT 1 FROM tags WHERE target_digest = sqlc.arg('digest'))
    OR EXISTS (SELECT 1 FROM files WHERE blob_digest = sqlc.arg('digest'))
    OR EXISTS (SELECT 1 FROM manifest_children WHERE child_digest = sqlc.arg('digest'))
    OR EXISTS (SELECT 1 FROM remote_cache WHERE blob_digest = sqlc.arg('digest'))
    OR EXISTS (SELECT 1 FROM evidence WHERE blob_digest = sqlc.arg('digest'))
    OR EXISTS (SELECT 1 FROM evidence WHERE subject_digest = sqlc.arg('digest'))
    OR EXISTS (SELECT 1 FROM lifecycle_events WHERE subject_digest = sqlc.arg('digest'))
)::bool AS referenced;

-- name: GCDeleteManifestEdges :exec
DELETE FROM manifest_children WHERE parent_digest = $1;

-- name: GCDeletePlacements :exec
DELETE FROM repository_blobs WHERE digest = $1;

-- name: GCDeleteSubjectOwners :exec
DELETE FROM subject_owners WHERE digest = $1;

-- name: GCDeleteProvenance :exec
DELETE FROM provenance WHERE digest = $1;

-- name: GCDeleteSubject :exec
DELETE FROM subjects WHERE digest = $1;

-- name: GCDeleteBlob :execrows
-- The foreign keys from tags, files, evidence, and remote_cache restrict, so
-- a reference the re-check somehow missed fails this delete loudly.
DELETE FROM blobs WHERE digest = $1;
