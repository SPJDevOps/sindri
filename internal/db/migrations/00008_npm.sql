-- +goose Up
-- +goose StatementBegin

-- Hosted npm (ADR 24). The protocol and subject-kind lists gain an entry, and
-- the dist-tag state and its history get tables of their own.

ALTER TABLE repositories DROP CONSTRAINT repositories_protocol_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_protocol_check
    CHECK (protocol IN ('oci', 'pypi', 'npm'));

ALTER TABLE subjects DROP CONSTRAINT subjects_kind_check;
ALTER TABLE subjects ADD CONSTRAINT subjects_kind_check
    CHECK (kind IN ('oci-manifest', 'oci-index', 'pypi-dist', 'file', 'npm-tarball'));

-- The deprecation message npm publishes per version; NULL means not deprecated.
ALTER TABLE versions ADD COLUMN deprecated text;

-- Mutable pointers from a tag to a version, per package. Deleting the package
-- or the version removes the tag.
CREATE TABLE dist_tags (
    package_id uuid NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    tag        text NOT NULL,
    version_id uuid NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    PRIMARY KEY (package_id, tag)
);

-- Who moved which tag, and when. APPEND ONLY, like lifecycle_events. No
-- foreign keys: history outlives the package and the version (ADR 24).
CREATE TABLE dist_tag_events (
    id           bigserial   PRIMARY KEY,
    repository   text        NOT NULL,
    package      text        NOT NULL,
    tag          text        NOT NULL,
    from_version text,
    to_version   text,
    actor        text        NOT NULL,
    at           timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION dist_tag_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'dist_tag_events is append-only'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER dist_tag_events_no_update_or_delete
    BEFORE UPDATE OR DELETE ON dist_tag_events
    FOR EACH ROW EXECUTE FUNCTION dist_tag_events_append_only();

CREATE TRIGGER dist_tag_events_no_truncate
    BEFORE TRUNCATE ON dist_tag_events
    FOR EACH STATEMENT EXECUTE FUNCTION dist_tag_events_append_only();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS dist_tag_events;
DROP FUNCTION IF EXISTS dist_tag_events_append_only();
DROP TABLE IF EXISTS dist_tags;
ALTER TABLE versions DROP COLUMN IF EXISTS deprecated;

-- NOT VALID: the old lists refuse new npm rows but do not re-check the npm
-- rows already present, so a rollback succeeds with data in place.
ALTER TABLE subjects DROP CONSTRAINT subjects_kind_check;
ALTER TABLE subjects ADD CONSTRAINT subjects_kind_check
    CHECK (kind IN ('oci-manifest', 'oci-index', 'pypi-dist', 'file')) NOT VALID;

ALTER TABLE repositories DROP CONSTRAINT repositories_protocol_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_protocol_check
    CHECK (protocol IN ('oci', 'pypi')) NOT VALID;

-- +goose StatementEnd
