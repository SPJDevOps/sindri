-- +goose Up
-- +goose StatementBegin

-- Digests an operator evicted from one OCI proxy repository and refuses to
-- fetch again (SIN-73, ADR 14). The row is the audit record: who blocked it,
-- why, and when. The digest is text in the same form as repository_blobs, and
-- has no foreign key to blobs: a block may name a digest never seen here, and
-- it must outlive the blob row the collector deletes. Additive: a binary one
-- version back never names the table.
CREATE TABLE repository_blocked_digests (
    repo_id    uuid        NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    digest     text        NOT NULL,
    blocked_by text        NOT NULL,
    reason     text        NOT NULL DEFAULT '',
    blocked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, digest)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS repository_blocked_digests;

-- +goose StatementEnd
