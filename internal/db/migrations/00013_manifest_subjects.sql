-- +goose Up
-- +goose StatementBegin

-- The facts the OCI referrers API lists (ADR 32): one row per manifest that
-- names a subject, keyed by the manifest's own digest. Everything here is read
-- from the manifest bytes, so it is the same in every repository; which
-- repository lists the row is decided by repository_blobs, never by this table
-- (invariant I2). subject_digest has no foreign key: the specification lets a
-- referrer arrive before the manifest it refers to. Additive: a binary one
-- version back never names the table. The garbage collector deletes a row with
-- its blob, because referrer_digest references blobs.
CREATE TABLE manifest_subjects (
    referrer_digest text   PRIMARY KEY REFERENCES blobs (digest),
    subject_digest  text   NOT NULL,
    media_type      text   NOT NULL,
    artifact_type   text   NOT NULL DEFAULT '',
    size            bigint NOT NULL,
    annotations     jsonb  NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX manifest_subjects_subject_idx ON manifest_subjects (subject_digest);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS manifest_subjects;

-- +goose StatementEnd
