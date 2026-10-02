-- +goose Up
-- +goose StatementBegin

-- Configuration as code and machine identity (M7, ADR 17 and ADR 18).
--
-- Both tables are additions, so the M6 binary runs against this schema
-- unchanged and a rollback needs no restore.
--
-- repositories.config (jsonb, from 00002) now carries what the reconciler and
-- RBAC read on the request path, so authorizing a pull costs nothing beyond
-- the repository lookup the adapter already makes:
--   {"namespace": "payments", "source": "file" | "apply",
--    "access": {"read": [...], "write": [...]},
--    "lifecycleStages": [...] | null}

-- The organisation policy is a singleton. A team applying only its own
-- namespace is validated against the policy the organisation last applied.
CREATE TABLE org_policy (
    id         smallint    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    spec       jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Machine tokens for CI. Only a SHA-256 of the token is stored: the token is
-- 32 random bytes, so a fast hash is a sufficient lookup key and a database
-- dump discloses no usable credential. A token is scoped to one repository,
-- one namespace, or (for an administrator) neither, and carries one role.
CREATE TABLE machine_tokens (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    token_hash bytea       NOT NULL,
    repo_id    uuid        REFERENCES repositories (id) ON DELETE CASCADE,
    namespace  text,
    role       text        NOT NULL CHECK (role IN ('read', 'write', 'admin')),
    created_by text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT machine_tokens_hash_unique UNIQUE (token_hash),
    CONSTRAINT machine_tokens_one_scope CHECK (repo_id IS NULL OR namespace IS NULL)
);

CREATE INDEX repositories_managed_by_idx ON repositories (managed_by);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS repositories_managed_by_idx;
DROP TABLE IF EXISTS machine_tokens;
DROP TABLE IF EXISTS org_policy;

-- +goose StatementEnd
