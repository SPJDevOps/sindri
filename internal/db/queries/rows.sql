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
           max(v.published_at)::timestamptz AS last_published_at
    FROM packages p
    JOIN versions v ON v.package_id = p.id
    WHERE p.repo_id = sqlc.arg('repo_id')
      AND (sqlc.arg('name_filter')::text = '' OR strpos(lower(p.normalized_name), lower(sqlc.arg('name_filter')::text)) > 0)
    GROUP BY p.id
)
SELECT m.id, m.normalized_name, m.display_name, m.version_count, lp.last_published_at,
       count(*) OVER ()::bigint AS total
FROM matched m
-- The join back to matched only tells sqlc the time may be null: a package with
-- no dated publish has none, and a bare time.Time could not scan that.
LEFT JOIN matched lp ON lp.id = m.id
ORDER BY (CASE WHEN sqlc.arg('by_published')::bool THEN m.last_published_at END) DESC NULLS LAST,
         m.normalized_name
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: ListPackageRowsOCI :many
-- The same for an OCI repository, where an image has tags and no versions rows.
-- Its last publish is the newest first-publish (subjects.created_at) of any
-- digest a tag points at; proxy content has no subject and so no time. A proxy
-- repository never reads subjects at all (hosted is false): a digest it caches
-- may also be pushed to a hosted repository the reader cannot see, and that
-- repository's push time must not reach this page (invariant I2).
WITH matched AS (
    SELECT p.id, p.normalized_name, p.display_name,
           count(t.name)::bigint AS tag_count,
           max(s.created_at)::timestamptz AS last_published_at
    FROM packages p
    LEFT JOIN tags t ON t.package_id = p.id AND t.repo_id = p.repo_id
    LEFT JOIN subjects s ON s.digest = t.target_digest AND sqlc.arg('hosted')::bool
    WHERE p.repo_id = sqlc.arg('repo_id')
      AND (sqlc.arg('name_filter')::text = '' OR strpos(lower(p.normalized_name), lower(sqlc.arg('name_filter')::text)) > 0)
    GROUP BY p.id
)
SELECT m.id, m.normalized_name, m.display_name, m.tag_count, lp.last_published_at,
       count(*) OVER ()::bigint AS total
FROM matched m
-- The join back to matched only tells sqlc the time may be null: a package with
-- no dated publish has none, and a bare time.Time could not scan that.
LEFT JOIN matched lp ON lp.id = m.id
ORDER BY (CASE WHEN sqlc.arg('by_published')::bool THEN m.last_published_at END) DESC NULLS LAST,
         m.normalized_name
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: ListVersionsForPackages :many
-- Every version string of the named packages, for choosing which is current
-- (the ordering is a protocol's, so it happens in Go).
SELECT v.package_id, v.version
FROM versions v
WHERE v.package_id = ANY(sqlc.arg('package_ids')::uuid[]);

-- name: ListLatestDistTags :many
-- The version each named package's "latest" dist-tag points at.
SELECT d.package_id, v.version
FROM dist_tags d
JOIN versions v ON v.id = d.version_id
WHERE d.package_id = ANY(sqlc.arg('package_ids')::uuid[])
  AND d.tag = 'latest';

-- name: ListLatestTagsForPackages :many
-- Per named image, the tag whose target was first published most recently.
-- A proxy repository (hosted is false) reads no subjects, so it has none: the
-- digest may also live in a hosted repository the reader cannot see (I2).
SELECT t.package_id,
       COALESCE((array_agg(t.name ORDER BY s.created_at DESC, t.name) FILTER (WHERE s.created_at IS NOT NULL))[1], '')::text AS latest_tag
FROM tags t
LEFT JOIN subjects s ON s.digest = t.target_digest AND sqlc.arg('hosted')::bool
WHERE t.package_id = ANY(sqlc.arg('package_ids')::uuid[])
GROUP BY t.package_id;

-- name: PackageFindingsOCI :many
-- Per named image, what the newest Sindri scans of its current subjects found,
-- where current means every digest a tag points at now. The work is done here
-- so its cost follows the database, not the number of tags shipped through the
-- caller. subjects is how many distinct digests are current and scanned how
-- many of them are fully scanned (every platform the newest scan lists has a
-- scan, ADR 30); the severity columns add up the
-- newest scan per platform of each. Only producer = sindri counts (ADR 29).
WITH cur AS (
    SELECT DISTINCT t.package_id, t.target_digest AS digest
    FROM tags t
    WHERE t.package_id = ANY(sqlc.arg('package_ids')::uuid[])
), newest AS (
    -- The newest Sindri scan per platform: the rule of ListNewestSindriScans,
    -- which the severity gate reads. Keep the two in step.
    SELECT DISTINCT ON (c.package_id, c.digest, coalesce(e.metadata->>'platform', ''))
           c.package_id, c.digest, coalesce(e.metadata->>'platform', '') AS platform,
           e.metadata->'severity_counts' AS counts, e.metadata->'platforms' AS platforms,
           e.created_at
    FROM cur c
    JOIN evidence e ON e.subject_digest = c.digest
                   AND e.type = 'scan'
                   AND e.metadata->>'producer' = 'sindri'
    ORDER BY c.package_id, c.digest, coalesce(e.metadata->>'platform', ''), e.created_at DESC, e.id DESC
), top AS (
    -- The newest scan overall lists the platforms an index must have a scan
    -- for, as the gate reads it (evaluateScanRule): newest created_at, the
    -- first platform by name on a tie.
    SELECT DISTINCT ON (package_id, digest) package_id, digest, platforms
    FROM newest
    ORDER BY package_id, digest, created_at DESC, platform
), complete AS (
    -- A digest is fully scanned when every platform that scan lists has a
    -- newest scan of its own; an index with one of two scanned is not.
    SELECT t.package_id, t.digest
    FROM top t
    WHERE NOT EXISTS (
        SELECT 1
        FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(t.platforms) = 'array' THEN t.platforms ELSE '[]'::jsonb END) AS want(platform)
        WHERE NOT EXISTS (
            SELECT 1 FROM newest n
            WHERE n.package_id = t.package_id AND n.digest = t.digest AND n.platform = want.platform
        )
    )
)
SELECT c.package_id,
       count(DISTINCT c.digest)::bigint AS subjects,
       count(DISTINCT k.digest)::bigint AS scanned,
       COALESCE(sum((n.counts->>'CRITICAL')::int), 0)::bigint AS critical,
       COALESCE(sum((n.counts->>'HIGH')::int), 0)::bigint AS high,
       COALESCE(sum((n.counts->>'MEDIUM')::int), 0)::bigint AS medium,
       COALESCE(sum((n.counts->>'LOW')::int), 0)::bigint AS low,
       COALESCE(sum((n.counts->>'UNKNOWN')::int), 0)::bigint AS unknown
FROM cur c
LEFT JOIN newest n ON n.package_id = c.package_id AND n.digest = c.digest
LEFT JOIN complete k ON k.package_id = c.package_id AND k.digest = c.digest
GROUP BY c.package_id;

-- name: PackageFindingsVersioned :many
-- The same for npm and PyPI, where current is one named version per package:
-- package_ids[i] is at versions[i]. Every file of the version is a subject.
WITH picked AS (
    SELECT a.package_id::uuid AS package_id, b.version::text AS version
    FROM unnest(sqlc.arg('package_ids')::uuid[]) WITH ORDINALITY AS a(package_id, n)
    JOIN unnest(sqlc.arg('versions')::text[]) WITH ORDINALITY AS b(version, n) ON b.n = a.n
), cur AS (
    SELECT DISTINCT p.package_id, f.blob_digest AS digest
    FROM picked p
    JOIN versions v ON v.package_id = p.package_id AND v.version = p.version
    JOIN files f ON f.version_id = v.id AND f.upstream_gone_at IS NULL
), newest AS (
    -- The newest Sindri scan per platform: the rule of ListNewestSindriScans,
    -- which the severity gate reads. Keep the two in step.
    SELECT DISTINCT ON (c.package_id, c.digest, coalesce(e.metadata->>'platform', ''))
           c.package_id, c.digest, coalesce(e.metadata->>'platform', '') AS platform,
           e.metadata->'severity_counts' AS counts, e.metadata->'platforms' AS platforms,
           e.created_at
    FROM cur c
    JOIN evidence e ON e.subject_digest = c.digest
                   AND e.type = 'scan'
                   AND e.metadata->>'producer' = 'sindri'
    ORDER BY c.package_id, c.digest, coalesce(e.metadata->>'platform', ''), e.created_at DESC, e.id DESC
), top AS (
    -- The newest scan overall lists the platforms an index must have a scan
    -- for, as the gate reads it (evaluateScanRule): newest created_at, the
    -- first platform by name on a tie.
    SELECT DISTINCT ON (package_id, digest) package_id, digest, platforms
    FROM newest
    ORDER BY package_id, digest, created_at DESC, platform
), complete AS (
    -- A digest is fully scanned when every platform that scan lists has a
    -- newest scan of its own; an index with one of two scanned is not.
    SELECT t.package_id, t.digest
    FROM top t
    WHERE NOT EXISTS (
        SELECT 1
        FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(t.platforms) = 'array' THEN t.platforms ELSE '[]'::jsonb END) AS want(platform)
        WHERE NOT EXISTS (
            SELECT 1 FROM newest n
            WHERE n.package_id = t.package_id AND n.digest = t.digest AND n.platform = want.platform
        )
    )
)
SELECT c.package_id,
       count(DISTINCT c.digest)::bigint AS subjects,
       count(DISTINCT k.digest)::bigint AS scanned,
       COALESCE(sum((n.counts->>'CRITICAL')::int), 0)::bigint AS critical,
       COALESCE(sum((n.counts->>'HIGH')::int), 0)::bigint AS high,
       COALESCE(sum((n.counts->>'MEDIUM')::int), 0)::bigint AS medium,
       COALESCE(sum((n.counts->>'LOW')::int), 0)::bigint AS low,
       COALESCE(sum((n.counts->>'UNKNOWN')::int), 0)::bigint AS unknown
FROM cur c
LEFT JOIN newest n ON n.package_id = c.package_id AND n.digest = c.digest
LEFT JOIN complete k ON k.package_id = c.package_id AND k.digest = c.digest
GROUP BY c.package_id;

-- name: RemoteCacheStats :one
-- What a proxy holds: the cached upstream answers and when the last successful
-- fetch was, null when it has fetched nothing (the join back to agg is how
-- sqlc learns that). A miss is a recorded 404, not content, and never counts.
WITH agg AS (
    SELECT count(*) FILTER (WHERE c.status = 'hit')::bigint AS entries,
           (max(c.fetched_at) FILTER (WHERE c.status = 'hit'))::timestamptz AS fetched_at
    FROM remote_cache c
    JOIN repositories r ON r.id = c.repo_id
    WHERE r.name = $1
)
SELECT a.entries, l.fetched_at AS last_fetched_at
FROM agg a
LEFT JOIN agg l ON true;

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
-- stages is the highest stage reached in each of repo_names, with its current
-- action (approve or revoke): the top position with any current state, the
-- rule the artifact page's verdict strip reads. A stage never reached is absent.
-- scans is the newest Sindri-produced scan per platform (ADR 29, ADR 30),
-- trimmed to what a summary needs, with its time and the platforms an index
-- scan lists; a client-attached scan never matches. The rule is that of
-- ListNewestSindriScans.
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
           SELECT jsonb_agg(jsonb_build_object('repository', st.repo_name, 'stage', st.stage_name, 'position', st.position, 'action', st.action))
           FROM (
               SELECT DISTINCT ON (r.name) r.name AS repo_name, ls.name AS stage_name, ls.position, lc.action
               FROM lifecycle_current lc
               JOIN lifecycle_stages ls ON ls.id = lc.stage_id
               JOIN repositories r ON r.id = lc.repo_id
               WHERE lc.subject_digest = w.digest
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
                          'platforms', e.metadata->'platforms',
                          'created_at', e.created_at,
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
