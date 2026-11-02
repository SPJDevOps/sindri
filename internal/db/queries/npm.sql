-- name: InsertVersionExclusive :one
-- Atomic create: a concurrent loser gets no row rather than the winner's.
INSERT INTO versions (package_id, version)
VALUES ($1, $2)
ON CONFLICT (package_id, version) DO NOTHING
RETURNING *;

-- name: ListNPMVersions :many
SELECT v.id AS version_id, v.version, v.deprecated,
       f.id, f.blob_digest, f.filename, f.media_type, f.metadata,
       b.size AS blob_size
FROM versions v
JOIN files f ON f.version_id = v.id
JOIN blobs b ON b.digest = f.blob_digest
WHERE v.package_id = $1
ORDER BY v.version, f.filename;

-- name: ListDistTags :many
SELECT dt.tag, v.version
FROM dist_tags dt
JOIN versions v ON v.id = dt.version_id
WHERE dt.package_id = $1
ORDER BY dt.tag;

-- name: UpsertDistTag :exec
INSERT INTO dist_tags (package_id, tag, version_id)
VALUES ($1, $2, $3)
ON CONFLICT (package_id, tag) DO UPDATE SET version_id = EXCLUDED.version_id;

-- name: DeleteDistTag :execrows
DELETE FROM dist_tags WHERE package_id = $1 AND tag = $2;

-- name: GetDistTagVersion :one
SELECT v.version
FROM dist_tags dt
JOIN versions v ON v.id = dt.version_id
WHERE dt.package_id = $1 AND dt.tag = $2;

-- name: InsertDistTagEvent :exec
INSERT INTO dist_tag_events (repository, package, tag, from_version, to_version, actor)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: SetVersionDeprecated :execrows
UPDATE versions SET deprecated = $2 WHERE id = $1;

-- name: ListDistTagsForVersion :many
SELECT tag FROM dist_tags WHERE version_id = $1 ORDER BY tag;

-- name: LockPackage :one
-- Serializes dist-tag changes and version deletes on one package, so the
-- history they append reads in the order the tags actually moved.
SELECT id FROM packages WHERE id = $1 FOR UPDATE;
