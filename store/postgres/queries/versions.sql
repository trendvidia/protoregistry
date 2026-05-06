-- name: InsertVersion :exec
INSERT INTO schema_versions (namespace_id, schema_id, version, compiled, compiler_version, created_by, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetVersion :one
SELECT namespace_id, schema_id, version, compiled, compiler_version,
       created_at, created_by, deleted_at, metadata
FROM schema_versions
WHERE namespace_id = $1 AND schema_id = $2 AND version = $3;

-- name: ListVersions :many
SELECT version
FROM schema_versions
WHERE namespace_id = $1 AND schema_id = $2 AND deleted_at IS NULL
ORDER BY version;

-- name: InsertVersionFile :exec
INSERT INTO schema_version_files (namespace_id, schema_id, version, filename, blob_sha256)
VALUES ($1, $2, $3, $4, $5);

-- name: GetVersionFiles :many
SELECT namespace_id, schema_id, version, filename, blob_sha256
FROM schema_version_files
WHERE namespace_id = $1 AND schema_id = $2 AND version = $3
ORDER BY filename;

-- name: InsertVersionDep :exec
INSERT INTO schema_version_deps (namespace_id, schema_id, version, dep_schema_id, dep_filename, dep_version)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetVersionDeps :many
SELECT namespace_id, schema_id, version, dep_schema_id, dep_filename, dep_version
FROM schema_version_deps
WHERE namespace_id = $1 AND schema_id = $2 AND version = $3;

-- name: GetDependents :many
SELECT DISTINCT schema_id, version
FROM schema_version_deps
WHERE namespace_id = $1 AND dep_schema_id = $2;

-- name: SoftDeleteVersion :exec
UPDATE schema_versions SET deleted_at = now()
WHERE namespace_id = $1 AND schema_id = $2 AND version = $3;
