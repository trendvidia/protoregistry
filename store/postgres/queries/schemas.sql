-- name: CreateSchema :exec
INSERT INTO schemas (namespace_id, schema_id, metadata)
VALUES ($1, $2, $3);

-- name: GetSchema :one
SELECT namespace_id, schema_id, current_version, staged_version,
       created_at, deleted_at, metadata
FROM schemas
WHERE namespace_id = $1 AND schema_id = $2;

-- name: ListSchemas :many
SELECT namespace_id, schema_id, current_version, staged_version,
       created_at, deleted_at, metadata
FROM schemas
WHERE namespace_id = $1 AND deleted_at IS NULL
ORDER BY schema_id;

-- ListSchemasPage is the keyset-paginated variant. Returns at most $3 schemas
-- in the namespace whose schema_id is strictly greater than $2, ordered by
-- schema_id. Pass an empty string for $2 to start at the beginning.
-- name: ListSchemasPage :many
SELECT namespace_id, schema_id, current_version, staged_version,
       created_at, deleted_at, metadata
FROM schemas
WHERE namespace_id = $1 AND deleted_at IS NULL
  AND schema_id > $2
ORDER BY schema_id
LIMIT $3;

-- name: GetSchemaForUpdate :one
SELECT namespace_id, schema_id, current_version, staged_version,
       created_at, deleted_at, metadata
FROM schemas
WHERE namespace_id = $1 AND schema_id = $2
FOR UPDATE;

-- name: SetStagedVersion :exec
UPDATE schemas SET staged_version = $3
WHERE namespace_id = $1 AND schema_id = $2;

-- name: ClearStagedVersion :exec
UPDATE schemas SET staged_version = NULL
WHERE namespace_id = $1 AND schema_id = $2;

-- name: PromoteAllStaged :many
UPDATE schemas
SET current_version = staged_version,
    staged_version = NULL
WHERE namespace_id = $1
  AND staged_version IS NOT NULL
RETURNING schema_id, current_version;

-- name: DiscardAllStaged :exec
UPDATE schemas
SET staged_version = NULL
WHERE namespace_id = $1
  AND staged_version IS NOT NULL;

-- name: SetCurrentVersion :exec
UPDATE schemas SET current_version = $3
WHERE namespace_id = $1 AND schema_id = $2;

-- name: SoftDeleteSchema :exec
UPDATE schemas SET deleted_at = now()
WHERE namespace_id = $1 AND schema_id = $2 AND deleted_at IS NULL;

-- name: GetStagedSchemas :many
SELECT namespace_id, schema_id, current_version, staged_version,
       created_at, deleted_at, metadata
FROM schemas
WHERE namespace_id = $1
  AND staged_version IS NOT NULL
  AND deleted_at IS NULL;
