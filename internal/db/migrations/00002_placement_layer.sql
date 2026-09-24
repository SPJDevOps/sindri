-- +goose Up
-- +goose StatementBegin

-- Placement layer (ARCHITECTURE.md section 5).
--
-- Blobs are content-addressed once (I1). Repository membership is metadata:
-- packages → versions → files point at digests; deleting a placement never
-- deletes bytes. Members, remotes, tags, and manifest_children are created
-- here so later milestones do not reshape the schema; M2 behaviour only
-- exercises hosted repositories and the file graph.

CREATE TABLE blobs (
    digest          text        PRIMARY KEY,
    size            bigint      NOT NULL CHECK (size >= 0),
    storage_backend text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE repositories (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    type       text        NOT NULL CHECK (type IN ('hosted', 'proxy', 'virtual')),
    protocol   text        NOT NULL CHECK (protocol IN ('oci', 'pypi')),
    config     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    locked     boolean     NOT NULL DEFAULT false,
    managed_by text        NOT NULL CHECK (managed_by IN ('config', 'api')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT repositories_name_unique UNIQUE (name)
);

CREATE TABLE repository_members (
    virtual_repo_id uuid    NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    member_repo_id  uuid    NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    position        integer NOT NULL CHECK (position >= 0),
    PRIMARY KEY (virtual_repo_id, member_repo_id),
    CONSTRAINT repository_members_distinct CHECK (virtual_repo_id <> member_repo_id)
);

CREATE UNIQUE INDEX repository_members_position_idx
    ON repository_members (virtual_repo_id, position);

CREATE TABLE remotes (
    repo_id              uuid PRIMARY KEY REFERENCES repositories (id) ON DELETE CASCADE,
    upstream_url         text NOT NULL,
    credentials_ref      text,
    ttl_seconds          integer NOT NULL DEFAULT 0 CHECK (ttl_seconds >= 0),
    negative_ttl_seconds integer NOT NULL DEFAULT 0 CHECK (negative_ttl_seconds >= 0)
);

CREATE TABLE packages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id         uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    normalized_name text NOT NULL,
    display_name    text NOT NULL,
    CONSTRAINT packages_repo_name_unique UNIQUE (repo_id, normalized_name)
);

CREATE TABLE versions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    package_id uuid NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    version    text NOT NULL,
    CONSTRAINT versions_package_version_unique UNIQUE (package_id, version)
);

CREATE TABLE files (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version_id  uuid NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    blob_digest text NOT NULL REFERENCES blobs (digest),
    filename    text NOT NULL,
    media_type  text NOT NULL DEFAULT 'application/octet-stream',
    metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT files_version_filename_unique UNIQUE (version_id, filename)
);

CREATE TABLE tags (
    repo_id       uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    package_id    uuid NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    name          text NOT NULL,
    target_digest text NOT NULL REFERENCES blobs (digest),
    PRIMARY KEY (repo_id, package_id, name)
);

CREATE TABLE manifest_children (
    parent_digest text NOT NULL REFERENCES blobs (digest),
    child_digest  text NOT NULL REFERENCES blobs (digest),
    PRIMARY KEY (parent_digest, child_digest),
    CONSTRAINT manifest_children_distinct CHECK (parent_digest <> child_digest)
);

-- Placement lookups: "is this digest linked into this repository?"
CREATE INDEX files_blob_digest_idx ON files (blob_digest);
CREATE INDEX packages_repo_id_idx ON packages (repo_id);
CREATE INDEX versions_package_id_idx ON versions (package_id);
CREATE INDEX tags_target_digest_idx ON tags (target_digest);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS manifest_children;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS versions;
DROP TABLE IF EXISTS packages;
DROP TABLE IF EXISTS remotes;
DROP TABLE IF EXISTS repository_members;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS blobs;

-- +goose StatementEnd
