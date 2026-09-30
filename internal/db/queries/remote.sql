-- name: UpsertRemote :one
INSERT INTO remotes (repo_id, upstream_url, credentials_ref, ttl_seconds, negative_ttl_seconds)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_id) DO UPDATE
SET upstream_url = EXCLUDED.upstream_url,
    credentials_ref = EXCLUDED.credentials_ref,
    ttl_seconds = EXCLUDED.ttl_seconds,
    negative_ttl_seconds = EXCLUDED.negative_ttl_seconds
RETURNING *;

-- name: GetRemote :one
SELECT * FROM remotes
WHERE repo_id = $1;

-- name: GetRemoteByRepoName :one
SELECT rem.* FROM remotes rem
JOIN repositories r ON r.id = rem.repo_id
WHERE r.name = $1;

-- name: InsertRepositoryMember :one
INSERT INTO repository_members (virtual_repo_id, member_repo_id, position)
VALUES ($1, $2, $3)
RETURNING *;

-- name: DeleteRepositoryMembers :exec
DELETE FROM repository_members
WHERE virtual_repo_id = $1;

-- name: ListRepositoryMembers :many
SELECT r.id, r.name, r.type, r.protocol, r.config, r.locked, r.managed_by, r.created_at,
       m.position
FROM repository_members m
JOIN repositories r ON r.id = m.member_repo_id
WHERE m.virtual_repo_id = $1
ORDER BY m.position;

-- name: ListRepositoryMembersByName :many
SELECT r.id, r.name, r.type, r.protocol, r.config, r.locked, r.managed_by, r.created_at,
       m.position
FROM repository_members m
JOIN repositories r ON r.id = m.member_repo_id
JOIN repositories v ON v.id = m.virtual_repo_id
WHERE v.name = $1
ORDER BY m.position;

-- name: PackageExistsInRepo :one
SELECT EXISTS (
    SELECT 1 FROM packages
    WHERE repo_id = $1 AND normalized_name = $2
)::bool AS exists;

-- name: UpsertRemoteCache :one
INSERT INTO remote_cache (repo_id, cache_key, status, blob_digest, fetched_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_id, cache_key) DO UPDATE
SET status = EXCLUDED.status,
    blob_digest = EXCLUDED.blob_digest,
    fetched_at = EXCLUDED.fetched_at
RETURNING *;

-- name: GetRemoteCache :one
SELECT * FROM remote_cache
WHERE repo_id = $1 AND cache_key = $2;

-- name: DeleteRemoteCache :execrows
DELETE FROM remote_cache
WHERE repo_id = $1 AND cache_key = $2;

-- name: ListRepositoryNamePrefixes :many
-- Returns every repository whose name is an exact prefix of the candidate
-- path (equal, or followed by '/'). Used by OCI longest-prefix routing.
SELECT * FROM repositories
WHERE $1 = name
   OR $1 LIKE name || '/%'
ORDER BY length(name) DESC;
