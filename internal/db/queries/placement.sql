-- name: CreateRepository :one
INSERT INTO repositories (name, type, protocol, config, locked, managed_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRepositoryByName :one
SELECT * FROM repositories
WHERE name = $1;

-- name: GetRepositoryByID :one
SELECT * FROM repositories
WHERE id = $1;

-- name: ListRepositories :many
SELECT * FROM repositories
WHERE (sqlc.narg('after_name')::text IS NULL OR name > sqlc.narg('after_name'))
ORDER BY name
LIMIT sqlc.arg('page_limit');

-- name: DeleteRepositoryByName :execrows
DELETE FROM repositories
WHERE name = $1;

-- name: UpsertBlob :one
INSERT INTO blobs (digest, size, storage_backend)
VALUES ($1, $2, $3)
ON CONFLICT (digest) DO UPDATE
SET size = EXCLUDED.size
RETURNING *;

-- name: GetBlob :one
SELECT * FROM blobs
WHERE digest = $1;

-- name: GetOrCreatePackage :one
INSERT INTO packages (repo_id, normalized_name, display_name)
VALUES ($1, $2, $3)
ON CONFLICT (repo_id, normalized_name) DO UPDATE
SET display_name = EXCLUDED.display_name
RETURNING *;

-- name: GetPackageByRepoAndName :one
SELECT * FROM packages
WHERE repo_id = $1 AND normalized_name = $2;

-- name: ListPackagesByRepo :many
SELECT * FROM packages
WHERE repo_id = sqlc.arg('repo_id')
  AND (sqlc.narg('after_name')::text IS NULL OR normalized_name > sqlc.narg('after_name'))
ORDER BY normalized_name
LIMIT sqlc.arg('page_limit');

-- name: GetOrCreateVersion :one
INSERT INTO versions (package_id, version)
VALUES ($1, $2)
ON CONFLICT (package_id, version) DO UPDATE
SET version = EXCLUDED.version
RETURNING *;

-- name: GetVersion :one
SELECT * FROM versions
WHERE package_id = $1 AND version = $2;

-- name: ListVersionsByPackage :many
SELECT * FROM versions
WHERE package_id = sqlc.arg('package_id')
  AND (sqlc.narg('after_version')::text IS NULL OR version > sqlc.narg('after_version'))
ORDER BY version
LIMIT sqlc.arg('page_limit');

-- name: UpsertFile :one
INSERT INTO files (version_id, blob_digest, filename, media_type, metadata)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (version_id, filename) DO UPDATE
SET blob_digest = EXCLUDED.blob_digest,
    media_type = EXCLUDED.media_type,
    metadata = EXCLUDED.metadata
RETURNING *;

-- name: GetFile :one
SELECT f.* FROM files f
JOIN versions v ON v.id = f.version_id
JOIN packages p ON p.id = v.package_id
JOIN repositories r ON r.id = p.repo_id
WHERE r.name = $1
  AND p.normalized_name = $2
  AND v.version = $3
  AND f.filename = $4;

-- name: DeleteFile :execrows
DELETE FROM files f
USING versions v, packages p, repositories r
WHERE f.version_id = v.id
  AND v.package_id = p.id
  AND p.repo_id = r.id
  AND r.name = $1
  AND p.normalized_name = $2
  AND v.version = $3
  AND f.filename = $4;

-- name: CountFilesByVersion :one
SELECT count(*)::bigint FROM files
WHERE version_id = $1;

-- name: DeleteVersionIfEmpty :execrows
DELETE FROM versions v
WHERE v.id = sqlc.arg('id')
  AND NOT EXISTS (SELECT 1 FROM files f WHERE f.version_id = sqlc.arg('id'));

-- name: CountVersionsByPackage :one
SELECT count(*)::bigint FROM versions
WHERE package_id = $1;

-- name: DeletePackageIfEmpty :execrows
DELETE FROM packages p
WHERE p.id = sqlc.arg('id')
  AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.package_id = sqlc.arg('id'));

-- name: CheckPlacement :one
-- Invariant I2: a digest is readable in a repository only when a placement
-- row (file or tag) links it into that repository's graph.
SELECT EXISTS (
    SELECT 1
    FROM files f
    JOIN versions v ON v.id = f.version_id
    JOIN packages p ON p.id = v.package_id
    JOIN repositories r ON r.id = p.repo_id
    WHERE r.name = sqlc.arg('repo_name')
      AND f.blob_digest = sqlc.arg('digest')
    UNION ALL
    SELECT 1
    FROM tags t
    JOIN repositories r ON r.id = t.repo_id
    WHERE r.name = sqlc.arg('repo_name')
      AND t.target_digest = sqlc.arg('digest')
)::bool AS placed;
