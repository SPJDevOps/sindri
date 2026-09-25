-- +goose Up
-- +goose StatementBegin

-- OCI blob membership: a digest uploaded (or written as a manifest) into a
-- repository is readable there under invariant I2 even before any tag points
-- at a graph that reaches it. manifest_children stays for GC reachability.
CREATE TABLE repository_blobs (
    repo_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    digest  text NOT NULL REFERENCES blobs (digest),
    PRIMARY KEY (repo_id, digest)
);

CREATE INDEX repository_blobs_digest_idx ON repository_blobs (digest);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS repository_blobs;

-- +goose StatementEnd
