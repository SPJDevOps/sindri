-- name: ListAllRepositories :many
SELECT * FROM repositories
ORDER BY name;

-- name: ListAllRepositoryMembers :many
SELECT v.name AS virtual_name, r.name AS member_name, m.position
FROM repository_members m
JOIN repositories v ON v.id = m.virtual_repo_id
JOIN repositories r ON r.id = m.member_repo_id
ORDER BY v.name, m.position;

-- name: UpdateRepositoryConfig :one
UPDATE repositories
SET config = $2, locked = $3
WHERE id = $1
RETURNING *;

-- name: DeleteRemote :exec
DELETE FROM remotes
WHERE repo_id = $1;

-- name: GetOrgPolicy :one
SELECT spec FROM org_policy
WHERE id = 1;

-- name: UpsertOrgPolicy :exec
INSERT INTO org_policy (id, spec, updated_at)
VALUES (1, $1, now())
ON CONFLICT (id) DO UPDATE
SET spec = EXCLUDED.spec, updated_at = now();
