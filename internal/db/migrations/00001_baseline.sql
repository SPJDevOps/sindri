-- +goose Up
-- +goose StatementBegin

-- Baseline.
--
-- Sindri's tables arrive with the milestones that own them: the placement layer
-- (repositories, packages, versions, files, blobs, tags, manifest_children) in
-- M2, and the identity layer (subjects, provenance, evidence, lifecycle_events)
-- in M6. See ARCHITECTURE.md section 5.
--
-- This migration exists so that the mechanism itself is exercised from the
-- first release: migrations are embedded in the binary, applied on startup
-- under an advisory lock, and recorded in goose_db_version. It records the
-- schema's origin rather than creating anything, which keeps it trivially
-- backward compatible for the one-version rollback the operations rules
-- require.
CREATE TABLE IF NOT EXISTS schema_origin (
    id          smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    initialized_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_origin (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS schema_origin;
-- +goose StatementEnd
