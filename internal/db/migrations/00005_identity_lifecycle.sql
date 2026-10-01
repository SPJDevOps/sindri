-- +goose Up
-- +goose StatementBegin

-- Identity layer (ARCHITECTURE.md section 5, ADR 4, ADR 16).
--
-- A subject is a digest we own: something published into a hosted repository.
-- Proxy-cached content never gets a subject (invariant I4). Ownership,
-- provenance, evidence, and lifecycle hang off the subject digest, never off a
-- repository path, so promotion moves no bytes (invariant I3).

CREATE TABLE subjects (
    digest     text        PRIMARY KEY REFERENCES blobs (digest),
    size       bigint      NOT NULL CHECK (size >= 0),
    kind       text        NOT NULL CHECK (kind IN ('oci-manifest', 'oci-index', 'pypi-dist', 'file')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE subject_owners (
    digest    text NOT NULL REFERENCES subjects (digest),
    owner_ref text NOT NULL,
    PRIMARY KEY (digest, owner_ref)
);

-- Provenance is what the publisher said about where the artifact came from.
-- v1 records it; it does not verify it (ADR 16). Republishing identical bytes
-- with identical claims is a no-op rather than a second row.
CREATE TABLE provenance (
    id          bigserial   PRIMARY KEY,
    digest      text        NOT NULL REFERENCES subjects (digest),
    source_type text        NOT NULL,
    source_ref  text,
    build_ref   text,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT provenance_claim_unique
        UNIQUE NULLS NOT DISTINCT (digest, source_type, source_ref, build_ref)
);

-- Evidence documents are blobs in the CAS like anything else. They have no
-- repository placement: their only read path is through the subject.
CREATE TABLE evidence (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_digest text        NOT NULL REFERENCES subjects (digest),
    type           text        NOT NULL CHECK (type IN ('sbom', 'signature', 'scan', 'attestation')),
    media_type     text        NOT NULL,
    blob_digest    text        NOT NULL REFERENCES blobs (digest),
    metadata       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     text        NOT NULL
);

CREATE INDEX evidence_subject_type_idx ON evidence (subject_digest, type);

-- Position is unique but deferred, so a reorder can swap two stages inside
-- one transaction.
CREATE TABLE lifecycle_stages (
    id       uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    name     text    NOT NULL,
    position integer NOT NULL CHECK (position >= 0),
    CONSTRAINT lifecycle_stages_name_unique UNIQUE (name),
    CONSTRAINT lifecycle_stages_position_unique UNIQUE (position) DEFERRABLE INITIALLY DEFERRED
);

-- Gates are configuration, replaced as a set; they are not the ledger.
CREATE TABLE promotion_gates (
    id       uuid  PRIMARY KEY DEFAULT gen_random_uuid(),
    stage_id uuid    NOT NULL REFERENCES lifecycle_stages (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position >= 0),
    rule     jsonb   NOT NULL,
    CONSTRAINT promotion_gates_stage_position_unique UNIQUE (stage_id, position)
);

-- The ledger of invariant I3. APPEND ONLY: the trigger below refuses UPDATE,
-- DELETE, and TRUNCATE. The foreign keys deliberately do not cascade, so a
-- repository or stage with history cannot be deleted out from under it.
CREATE TABLE lifecycle_events (
    id             bigserial   PRIMARY KEY,
    subject_digest text        NOT NULL REFERENCES subjects (digest),
    repo_id        uuid        NOT NULL REFERENCES repositories (id),
    stage_id       uuid        NOT NULL REFERENCES lifecycle_stages (id),
    action         text        NOT NULL CHECK (action IN ('approve', 'revoke')),
    actor          text        NOT NULL,
    reason         text        NOT NULL DEFAULT '',
    at             timestamptz NOT NULL DEFAULT now()
);

-- Serves both the current-state view and the decisions query: every lookup
-- names the digest and the stage.
CREATE INDEX lifecycle_events_current_idx
    ON lifecycle_events (subject_digest, stage_id, repo_id, at DESC, id DESC);

CREATE FUNCTION lifecycle_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'lifecycle_events is append-only (invariant I3)'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER lifecycle_events_no_update_or_delete
    BEFORE UPDATE OR DELETE ON lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION lifecycle_events_append_only();

CREATE TRIGGER lifecycle_events_no_truncate
    BEFORE TRUNCATE ON lifecycle_events
    FOR EACH STATEMENT EXECUTE FUNCTION lifecycle_events_append_only();

-- Current state: the latest event per (subject, repository, stage). A filter on
-- the DISTINCT ON columns is pushed down into lifecycle_events_current_idx.
CREATE VIEW lifecycle_current AS
SELECT DISTINCT ON (subject_digest, stage_id, repo_id)
    id, subject_digest, repo_id, stage_id, action, actor, reason, at
FROM lifecycle_events
ORDER BY subject_digest, stage_id, repo_id, at DESC, id DESC;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP VIEW IF EXISTS lifecycle_current;
DROP TABLE IF EXISTS lifecycle_events;
DROP FUNCTION IF EXISTS lifecycle_events_append_only();
DROP TABLE IF EXISTS promotion_gates;
DROP TABLE IF EXISTS lifecycle_stages;
DROP TABLE IF EXISTS evidence;
DROP TABLE IF EXISTS provenance;
DROP TABLE IF EXISTS subject_owners;
DROP TABLE IF EXISTS subjects;

-- +goose StatementEnd
