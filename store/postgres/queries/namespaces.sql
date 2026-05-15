-- name: CreateNamespace :exec
INSERT INTO namespaces (id, metadata, parent_namespace_id)
VALUES ($1, $2, $3);

-- name: GetNamespace :one
SELECT id, created_at, deleted_at, metadata, parent_namespace_id
FROM namespaces
WHERE id = $1;

-- name: ListNamespaces :many
SELECT id, created_at, deleted_at, metadata, parent_namespace_id
FROM namespaces
WHERE deleted_at IS NULL
ORDER BY id;

-- ListNamespacesPage returns at most $2 namespaces whose id is strictly
-- greater than $1, ordered by id. Pass an empty string for $1 to start at
-- the beginning. Keyset pagination: stable under concurrent inserts/deletes.
-- name: ListNamespacesPage :many
SELECT id, created_at, deleted_at, metadata, parent_namespace_id
FROM namespaces
WHERE deleted_at IS NULL
  AND id > $1
ORDER BY id
LIMIT $2;

-- name: SoftDeleteNamespace :exec
UPDATE namespaces SET deleted_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- SetNamespaceParent atomically sets a namespace's parent with cycle
-- prevention via a recursive CTE. Returns the number of affected rows: 0
-- means either the namespace doesn't exist, the parent doesn't exist, or
-- setting this parent would create a cycle (including self-reference).
-- Callers should treat 0 as a logical failure and report accordingly.
--
-- The depth bound (64) is a safety guard: PostgreSQL does not auto-detect
-- cycles in recursive CTEs and would loop indefinitely on a pre-existing
-- cycle. 64 is far beyond any reasonable hierarchy depth.
--
-- name: SetNamespaceParent :execrows
WITH RECURSIVE ancestors AS (
    SELECT id, parent_namespace_id, 1 AS depth
    FROM namespaces
    WHERE id = @new_parent_id::text
    UNION ALL
    SELECT n.id, n.parent_namespace_id, a.depth + 1
    FROM namespaces n
    JOIN ancestors a ON n.id = a.parent_namespace_id
    WHERE a.depth < 64
)
UPDATE namespaces
SET parent_namespace_id = @new_parent_id::text
WHERE id = @namespace_id::text
  AND @namespace_id::text <> @new_parent_id::text
  AND EXISTS (SELECT 1 FROM namespaces WHERE id = @new_parent_id::text)
  AND NOT EXISTS (SELECT 1 FROM ancestors WHERE id = @namespace_id::text);

-- ClearNamespaceParent removes the parent linkage (resets to NULL, which
-- is the implicit root). No cycle check needed.
-- name: ClearNamespaceParent :execrows
UPDATE namespaces
SET parent_namespace_id = NULL
WHERE id = $1;
