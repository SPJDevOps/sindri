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

-- name: ListEvictableCacheEntries :many
-- Proxy cache entries retention may remove (ADR 19). A hit is evictable once
-- it is older than the repository's TTL plus a stale-serving allowance, so an
-- upstream outage shorter than that allowance still serves the cached copy.
-- A TTL of zero means "fresh forever" and is never evicted. A miss is
-- evictable once its negative TTL has run out, at which point it already
-- behaves as no entry at all.
SELECT rc.repo_id, rc.cache_key, rc.status, r.name AS repo_name, r.protocol
FROM remote_cache rc
JOIN repositories r ON r.id = rc.repo_id AND r.type = 'proxy'
JOIN remotes rem ON rem.repo_id = rc.repo_id
WHERE (rc.status = 'hit' AND rem.ttl_seconds > 0
       AND rc.fetched_at < now() - ((rem.ttl_seconds + sqlc.arg('stale_seconds')::bigint) * interval '1 second'))
   OR (rc.status = 'miss'
       AND rc.fetched_at < now() - (rem.negative_ttl_seconds * interval '1 second'))
ORDER BY rc.fetched_at, rc.repo_id, rc.cache_key
LIMIT sqlc.arg('page_limit');

-- name: LockEvictableCacheEntry :one
-- Re-reads one entry under a row lock with the same predicate, so a refresh
-- that committed after the listing wins and the entry is kept.
SELECT rc.repo_id, rc.cache_key, rc.status, rc.blob_digest
FROM remote_cache rc
JOIN repositories r ON r.id = rc.repo_id AND r.type = 'proxy'
JOIN remotes rem ON rem.repo_id = rc.repo_id
WHERE rc.repo_id = sqlc.arg('repo_id') AND rc.cache_key = sqlc.arg('cache_key')
  AND ((rc.status = 'hit' AND rem.ttl_seconds > 0
        AND rc.fetched_at < now() - ((rem.ttl_seconds + sqlc.arg('stale_seconds')::bigint) * interval '1 second'))
    OR (rc.status = 'miss'
        AND rc.fetched_at < now() - (rem.negative_ttl_seconds * interval '1 second')))
FOR UPDATE OF rc SKIP LOCKED;

-- name: ListProxyPackageVersions :many
-- The versions of one project in a proxy repository, whose cached files
-- retention removes together with the index entry that justified them.
SELECT v.id AS version_id, p.id AS package_id
FROM versions v
JOIN packages p ON p.id = v.package_id
JOIN repositories r ON r.id = p.repo_id
WHERE r.id = sqlc.arg('repo_id')
  AND r.type = 'proxy'
  AND p.normalized_name = sqlc.arg('normalized_name');

-- name: DeleteFilesByVersion :execrows
DELETE FROM files
WHERE version_id = $1;

-- name: DeleteProxyTag :execrows
DELETE FROM tags t
USING repositories r, packages p
WHERE t.repo_id = r.id
  AND t.package_id = p.id
  AND r.id = sqlc.arg('repo_id')
  AND r.type = 'proxy'
  AND p.normalized_name = sqlc.arg('normalized_name')
  AND t.name = sqlc.arg('tag_name');
