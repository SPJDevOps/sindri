-- +goose Up
-- +goose StatementBegin

-- Garbage collection and the background worker (ADR 19, ADR 20).
--
-- Additive only: an M7 binary keeps working against this schema, so a
-- rollback needs no restore (CLAUDE.md, operational rules).

-- last_seen_at is the write intent. Every writer touches it before it writes
-- bytes or relies on bytes it found, and the collector never deletes a blob
-- seen within its grace period. Existing rows take the migration time rather
-- than created_at: nothing is collectable until one grace period after the
-- upgrade, which is the conservative answer for rows no intent ever covered.
ALTER TABLE blobs ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now();

-- The sweep re-checks every inbound reference to one candidate, so each
-- reference needs an index that leads with the referenced digest.
CREATE INDEX manifest_children_child_idx ON manifest_children (child_digest);
CREATE INDEX evidence_blob_digest_idx ON evidence (blob_digest);
CREATE INDEX remote_cache_blob_digest_idx ON remote_cache (blob_digest)
    WHERE blob_digest IS NOT NULL;

-- The audit endpoint pages the ledger newest first, per repository.
CREATE INDEX lifecycle_events_repo_id_idx ON lifecycle_events (repo_id, id DESC);

-- The queue table. Background work goes through here even while the worker
-- runs in the serving process, which is what keeps the post-v1 split serve
-- modes a configuration change (ARCHITECTURE.md section 9).
--
-- kind carries no CHECK on purpose: a binary that was rolled back must leave
-- the kinds it does not know alone rather than fail to start.
CREATE TABLE jobs (
    id               bigserial   PRIMARY KEY,
    kind             text        NOT NULL,
    args             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    state            text        NOT NULL DEFAULT 'queued'
                                 CHECK (state IN ('queued', 'running', 'succeeded', 'failed')),
    dedupe_key       text,
    attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts     integer     NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    run_after        timestamptz NOT NULL DEFAULT now(),
    lease_owner      text,
    lease_expires_at timestamptz,
    created_by       text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    last_error       text,
    result           jsonb
);

-- At most one active job per dedupe key. Enqueueing a key that is already
-- queued or running returns the existing job, and two schedulers racing each
-- other insert one row between them.
CREATE UNIQUE INDEX jobs_active_dedupe_idx ON jobs (dedupe_key)
    WHERE state IN ('queued', 'running');

CREATE INDEX jobs_claimable_idx ON jobs (run_after, id) WHERE state = 'queued';
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE state = 'running';
CREATE INDEX jobs_kind_finished_idx ON jobs (kind, finished_at DESC) WHERE state = 'succeeded';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS jobs;
DROP INDEX IF EXISTS lifecycle_events_repo_id_idx;
DROP INDEX IF EXISTS remote_cache_blob_digest_idx;
DROP INDEX IF EXISTS evidence_blob_digest_idx;
DROP INDEX IF EXISTS manifest_children_child_idx;
ALTER TABLE blobs DROP COLUMN IF EXISTS last_seen_at;

-- +goose StatementEnd
