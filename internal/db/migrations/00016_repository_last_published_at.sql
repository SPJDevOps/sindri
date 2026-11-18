-- +goose Up
-- +goose StatementBegin

-- The last time a version was published into a repository, kept on the
-- repository row so the repository list reads it instead of computing a max
-- over every version of every repository on the page (SIN-116: that join cost
-- ~17ms of a ~21ms query on the 50,000-package seed and broke the page budget
-- on a loaded CI runner).
--
-- NULL means nothing has been published here, or only versions from before
-- versions.published_at existed (00011), whose real time is not known.
--
-- The meaning is "the last time something was published here", not "the
-- newest version that survives": deleting a version (retention, GC, revoke)
-- leaves the value alone.
--
-- A trigger keeps it current rather than application code, so that every path
-- that inserts a versions row is covered: hosted publishes for OCI, PyPI and
-- npm, proxy cache fills, and a path a later change adds. It also stays right
-- if a v1.7.0 binary, which does not know the column, publishes after a
-- rollback. The update is skipped for a NULL published_at and when the value
-- would not increase, so re-publishing older data does not rewrite the row.
--
-- Backward compatible: the column is nullable and v1.7.0 never names it.
ALTER TABLE repositories ADD COLUMN last_published_at timestamptz NULL;

UPDATE repositories r
SET last_published_at = s.newest
FROM (
    SELECT p.repo_id, max(v.published_at) AS newest
    FROM versions v
    JOIN packages p ON p.id = v.package_id
    GROUP BY p.repo_id
) s
WHERE s.repo_id = r.id AND s.newest IS NOT NULL;

CREATE FUNCTION repositories_note_publish() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.published_at IS NULL THEN
        RETURN NULL;
    END IF;
    UPDATE repositories r
    SET last_published_at = NEW.published_at
    FROM packages p
    WHERE p.id = NEW.package_id
      AND r.id = p.repo_id
      AND (r.last_published_at IS NULL OR r.last_published_at < NEW.published_at);
    RETURN NULL;
END;
$$;

CREATE TRIGGER versions_note_publish
AFTER INSERT ON versions
FOR EACH ROW EXECUTE FUNCTION repositories_note_publish();

-- An OCI image has no versions row (it is a package plus tags), so a push is a
-- tag insert or move. The same rule applies there with now() as the time.
-- Existing OCI repositories stay NULL after the backfill: tags carry no
-- timestamp, so their real time is not known. Pushing by digest alone, which
-- creates no tag, is not a publish for this purpose.
CREATE FUNCTION repositories_note_tag_publish() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE repositories r
    SET last_published_at = now()
    WHERE r.id = NEW.repo_id
      AND (r.last_published_at IS NULL OR r.last_published_at < now());
    RETURN NULL;
END;
$$;

CREATE TRIGGER tags_note_publish
AFTER INSERT OR UPDATE OF target_digest ON tags
FOR EACH ROW EXECUTE FUNCTION repositories_note_tag_publish();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS tags_note_publish ON tags;
DROP FUNCTION IF EXISTS repositories_note_tag_publish();
DROP TRIGGER IF EXISTS versions_note_publish ON versions;
DROP FUNCTION IF EXISTS repositories_note_publish();
ALTER TABLE repositories DROP COLUMN IF EXISTS last_published_at;

-- +goose StatementEnd
