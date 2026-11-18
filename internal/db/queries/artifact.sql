-- Queries behind the artifact page's "names that point here" and "also placed
-- in" lines. Each is one query however many names or repositories there are.

-- name: ListNamesForDigest :many
-- The names inside one repository that point at a digest: the tags of an OCI
-- image, and the version and filename of an npm or PyPI file. kind is 'tag' or
-- 'file'; ref is the tag name or the version. Both arms use the digest indexes
-- (tags_target_digest_idx, files_blob_digest_idx). A file the upstream no
-- longer lists is not a placement (CheckPlacement) and so is not a name.
(SELECT 'tag'::text AS kind, p.normalized_name AS package, p.display_name AS display_name,
        t.name AS ref, ''::text AS filename
 FROM tags t
 JOIN repositories r ON r.id = t.repo_id
 JOIN packages p ON p.id = t.package_id
 WHERE r.name = sqlc.arg('repo_name') AND t.target_digest = sqlc.arg('digest')
 ORDER BY p.normalized_name, t.name
 LIMIT sqlc.arg('max_names'))
UNION ALL
(SELECT 'file'::text, p.normalized_name, p.display_name, v.version, f.filename
 FROM files f
 JOIN versions v ON v.id = f.version_id
 JOIN packages p ON p.id = v.package_id
 JOIN repositories r ON r.id = p.repo_id
 WHERE r.name = sqlc.arg('repo_name') AND f.blob_digest = sqlc.arg('digest')
   AND f.upstream_gone_at IS NULL
 ORDER BY p.normalized_name, v.version, f.filename
 LIMIT sqlc.arg('max_names'));

-- name: ListPlacingRepositories :many
-- The repositories among repo_names that place a digest, by the predicate of
-- CheckPlacement (invariant I2). The caller passes only repositories its reader
-- may read, so the limit applies after the readability filter and no repository
-- outside the list can influence, name, or count in the result.
SELECT r.name
FROM repositories r
WHERE r.name = ANY(sqlc.arg('repo_names')::text[]) AND (EXISTS (
    SELECT 1 FROM files f
    JOIN versions v ON v.id = f.version_id
    JOIN packages p ON p.id = v.package_id
    WHERE p.repo_id = r.id AND f.blob_digest = sqlc.arg('digest') AND f.upstream_gone_at IS NULL
) OR EXISTS (
    SELECT 1 FROM tags t WHERE t.repo_id = r.id AND t.target_digest = sqlc.arg('digest')
) OR EXISTS (
    SELECT 1 FROM repository_blobs rb WHERE rb.repo_id = r.id AND rb.digest = sqlc.arg('digest')
))
ORDER BY r.name
LIMIT sqlc.arg('max_repos');
