-- Queries behind the UI's repository and package pages. Each returns one page
-- of rows with the figures a row shows, in a constant number of queries however
-- many rows the page lists.

-- name: ListPackageRowsVersioned :many
-- One row per package of an npm or PyPI repository that has at least one
-- version: the version count and the newest published_at. A name kept only as
-- a claim, with no versions, is not listed (ListPackagesByRepo does the same).
-- total is the number of matching packages across every page.
WITH matched AS (
    SELECT p.id, p.normalized_name, p.display_name,
           count(v.id)::bigint AS version_count,
           max(v.published_at) AS last_published_at
    FROM packages p
    JOIN versions v ON v.package_id = p.id
    WHERE p.repo_id = sqlc.arg('repo_id')
      AND (sqlc.arg('name_filter')::text = '' OR strpos(lower(p.normalized_name), lower(sqlc.arg('name_filter')::text)) > 0)
    GROUP BY p.id
)
SELECT m.id, m.normalized_name, m.display_name, m.version_count, m.last_published_at,
       count(*) OVER ()::bigint AS total
FROM matched m
ORDER BY (CASE WHEN sqlc.arg('by_published')::bool THEN m.last_published_at END) DESC NULLS LAST,
         m.normalized_name
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: ListPackageRowsOCI :many
-- The same for an OCI repository, where an image has tags and no versions rows.
-- Its last publish is the newest first-publish (subjects.created_at) of any
-- digest a tag points at; proxy content has no subject and so no time.
WITH matched AS (
    SELECT p.id, p.normalized_name, p.display_name,
           count(t.name)::bigint AS tag_count,
           max(s.created_at) AS last_published_at
    FROM packages p
    LEFT JOIN tags t ON t.package_id = p.id AND t.repo_id = p.repo_id
    LEFT JOIN subjects s ON s.digest = t.target_digest
    WHERE p.repo_id = sqlc.arg('repo_id')
      AND (sqlc.arg('name_filter')::text = '' OR strpos(lower(p.normalized_name), lower(sqlc.arg('name_filter')::text)) > 0)
    GROUP BY p.id
)
SELECT m.id, m.normalized_name, m.display_name, m.tag_count, m.last_published_at,
       count(*) OVER ()::bigint AS total
FROM matched m
ORDER BY (CASE WHEN sqlc.arg('by_published')::bool THEN m.last_published_at END) DESC NULLS LAST,
         m.normalized_name
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: ListVersionDigestsForPackages :many
-- Every version of the named packages with the digests of its files, for
-- choosing which version is current. Files flagged gone upstream are not
-- listed, as everywhere else.
SELECT v.package_id, v.version,
       COALESCE(array_agg(f.blob_digest ORDER BY f.filename) FILTER (WHERE f.blob_digest IS NOT NULL), '{}')::text[] AS digests
FROM versions v
LEFT JOIN files f ON f.version_id = v.id AND f.upstream_gone_at IS NULL
WHERE v.package_id = ANY(sqlc.arg('package_ids')::uuid[])
GROUP BY v.id;

-- name: ListLatestDistTags :many
-- The version each named package's "latest" dist-tag points at.
SELECT d.package_id, v.version
FROM dist_tags d
JOIN versions v ON v.id = d.version_id
WHERE d.package_id = ANY(sqlc.arg('package_ids')::uuid[])
  AND d.tag = 'latest';

-- name: ListTagSummariesForPackages :many
-- Per named image: the digests its tags point at, and the tag whose target was
-- first published most recently. Tags of proxy content point at digests with no
-- subject, so they have no latest tag.
SELECT t.package_id,
       array_agg(DISTINCT t.target_digest)::text[] AS digests,
       COALESCE((array_agg(t.name ORDER BY s.created_at DESC, t.name) FILTER (WHERE s.created_at IS NOT NULL))[1], '')::text AS latest_tag
FROM tags t
LEFT JOIN subjects s ON s.digest = t.target_digest
WHERE t.package_id = ANY(sqlc.arg('package_ids')::uuid[])
GROUP BY t.package_id;

-- name: RemoteCacheStats :one
-- What a proxy holds: the cached upstream answers and when the last successful
-- fetch was. A miss is a recorded 404, not content, and never counts.
SELECT count(*) FILTER (WHERE c.status = 'hit')::bigint AS entries,
       max(c.fetched_at) FILTER (WHERE c.status = 'hit') AS last_fetched_at
FROM remote_cache c
JOIN repositories r ON r.id = c.repo_id
WHERE r.name = $1;

-- name: SummarizeSubjects :many
-- The figures a row or a header shows about each named digest, in one query.
-- The caller passes only digests it has already found placed in a repository
-- the caller may read (invariant I2); nothing here checks placement.
--
-- total_size is the digest's own blob plus the blobs it names, two levels deep
-- (an image's config and layers, or an index's manifests and theirs). A digest
-- that is not a subject (proxy content, a layer) has no created_at and no
-- owners, states, or scans.
--
-- stages is the highest approved stage in each of repo_names where the digest
-- is currently approved; a revoked or never-approved stage does not count.
-- scans is the newest Sindri-produced scan per platform (ADR 29, ADR 30),
-- trimmed to what a summary needs; a client-attached scan never matches.
SELECT w.digest::text AS digest,
       (b.size + COALESCE((
           SELECT sum(cb.size)
           FROM (
               SELECT mc.child_digest AS digest
               FROM manifest_children mc
               WHERE mc.parent_digest = w.digest
               UNION
               SELECT mc2.child_digest
               FROM manifest_children mc
               JOIN manifest_children mc2 ON mc2.parent_digest = mc.child_digest
               WHERE mc.parent_digest = w.digest
           ) kids
           JOIN blobs cb ON cb.digest = kids.digest
       ), 0))::bigint AS total_size,
       s.created_at AS created_at,
       COALESCE((
           SELECT array_agg(o.owner_ref ORDER BY o.owner_ref)
           FROM subject_owners o
           WHERE o.digest = w.digest
       ), '{}')::text[] AS owners,
       COALESCE((
           SELECT jsonb_agg(jsonb_build_object('repository', st.repo_name, 'stage', st.stage_name, 'position', st.position))
           FROM (
               SELECT DISTINCT ON (r.name) r.name AS repo_name, ls.name AS stage_name, ls.position
               FROM lifecycle_current lc
               JOIN lifecycle_stages ls ON ls.id = lc.stage_id
               JOIN repositories r ON r.id = lc.repo_id
               WHERE lc.subject_digest = w.digest
                 AND lc.action = 'approve'
                 AND r.name = ANY(sqlc.arg('repo_names')::text[])
               ORDER BY r.name, ls.position DESC
           ) st
       ), '[]'::jsonb)::jsonb AS stages,
       COALESCE((
           SELECT jsonb_agg(sc.summary)
           FROM (
               SELECT DISTINCT ON (coalesce(e.metadata->>'platform', ''))
                      jsonb_build_object(
                          'producer', e.metadata->'producer',
                          'platform', e.metadata->'platform',
                          'severity_counts', e.metadata->'severity_counts') AS summary
               FROM evidence e
               WHERE e.subject_digest = w.digest
                 AND e.type = 'scan'
                 AND e.metadata->>'producer' = 'sindri'
               ORDER BY coalesce(e.metadata->>'platform', ''), e.created_at DESC, e.id DESC
           ) sc
       ), '[]'::jsonb)::jsonb AS scans
FROM (SELECT DISTINCT d AS digest FROM unnest(sqlc.arg('digests')::text[]) AS d) w
JOIN blobs b ON b.digest = w.digest
LEFT JOIN subjects s ON s.digest = w.digest;
