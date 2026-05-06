-- name: CreateNamespace :exec
INSERT INTO namespaces (id, metadata)
VALUES ($1, $2);

-- name: GetNamespace :one
SELECT id, created_at, deleted_at, metadata
FROM namespaces
WHERE id = $1;

-- name: ListNamespaces :many
SELECT id, created_at, deleted_at, metadata
FROM namespaces
WHERE deleted_at IS NULL
ORDER BY id;

-- ListNamespacesPage returns at most $2 namespaces whose id is strictly
-- greater than $1, ordered by id. Pass an empty string for $1 to start at
-- the beginning. Keyset pagination: stable under concurrent inserts/deletes.
-- name: ListNamespacesPage :many
SELECT id, created_at, deleted_at, metadata
FROM namespaces
WHERE deleted_at IS NULL
  AND id > $1
ORDER BY id
LIMIT $2;

-- name: SoftDeleteNamespace :exec
UPDATE namespaces SET deleted_at = now()
WHERE id = $1 AND deleted_at IS NULL;
