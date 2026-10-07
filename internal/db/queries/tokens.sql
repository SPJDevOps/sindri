-- name: CreateMachineToken :one
INSERT INTO machine_tokens (name, token_hash, repo_id, namespace, role, created_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetMachineTokenByHash :one
SELECT t.id, t.name, t.repo_id, t.namespace, t.role, t.expires_at, t.revoked_at,
       r.name AS repo_name
FROM machine_tokens t
LEFT JOIN repositories r ON r.id = t.repo_id
WHERE t.token_hash = $1;

-- name: ListMachineTokens :many
SELECT t.id, t.name, t.repo_id, t.namespace, t.role, t.created_by, t.created_at,
       t.expires_at, t.revoked_at, r.name AS repo_name
FROM machine_tokens t
LEFT JOIN repositories r ON r.id = t.repo_id
ORDER BY t.created_at, t.id;

-- name: RevokeMachineToken :execrows
UPDATE machine_tokens
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;
