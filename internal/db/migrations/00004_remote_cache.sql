-- +goose Up
-- +goose StatementBegin

-- Proxy metadata cache (ARCHITECTURE.md section 4).
--
-- hit  = a successful upstream fetch whose local placement is still fresh.
-- miss = an upstream 404. A 5xx, timeout, or truncated body must never write
-- this row, and must never replace a previous hit (M5 roadmap watch-out).
--
-- Blob bytes live in the CAS and do not expire here; only the metadata key
-- (PyPI project index, OCI tag) is timed. Eviction of expired rows is M8.

CREATE TABLE remote_cache (
    repo_id     uuid        NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    cache_key   text        NOT NULL,
    status      text        NOT NULL CHECK (status IN ('hit', 'miss')),
    -- hit rows may point at a stored upstream metadata document (PyPI simple
    -- JSON, OCI manifest body). miss rows leave this null.
    blob_digest text        REFERENCES blobs (digest),
    fetched_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, cache_key),
    CONSTRAINT remote_cache_hit_digest CHECK (
        (status = 'miss' AND blob_digest IS NULL) OR status = 'hit'
    )
);

CREATE INDEX remote_cache_fetched_at_idx ON remote_cache (fetched_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS remote_cache;

-- +goose StatementEnd
