-- name: LoadAllCurrent :many
SELECT
    sv.namespace_id,
    sv.schema_id,
    sv.version,
    sv.compiled,
    sv.compiler_version,
    svf.filename,
    svf.blob_sha256
FROM schemas s
JOIN schema_versions sv
  ON sv.namespace_id = s.namespace_id
 AND sv.schema_id = s.schema_id
 AND sv.version = s.current_version
JOIN schema_version_files svf
  ON svf.namespace_id = sv.namespace_id
 AND svf.schema_id = sv.schema_id
 AND svf.version = sv.version
WHERE s.current_version IS NOT NULL
  AND s.deleted_at IS NULL
ORDER BY sv.namespace_id, sv.schema_id, svf.filename;

-- name: LoadNamespaceCurrent :many
SELECT
    sv.namespace_id,
    sv.schema_id,
    sv.version,
    sv.compiled,
    sv.compiler_version,
    svf.filename,
    svf.blob_sha256
FROM schemas s
JOIN schema_versions sv
  ON sv.namespace_id = s.namespace_id
 AND sv.schema_id = s.schema_id
 AND sv.version = s.current_version
JOIN schema_version_files svf
  ON svf.namespace_id = sv.namespace_id
 AND svf.schema_id = sv.schema_id
 AND svf.version = sv.version
WHERE s.namespace_id = $1
  AND s.current_version IS NOT NULL
  AND s.deleted_at IS NULL
ORDER BY sv.schema_id, svf.filename;

-- name: LoadNamespaceProposed :many
SELECT
    sv.namespace_id,
    sv.schema_id,
    sv.version,
    sv.compiled,
    sv.compiler_version,
    svf.filename,
    svf.blob_sha256
FROM schemas s
JOIN schema_versions sv
  ON sv.namespace_id = s.namespace_id
 AND sv.schema_id = s.schema_id
 AND sv.version = COALESCE(s.staged_version, s.current_version)
JOIN schema_version_files svf
  ON svf.namespace_id = sv.namespace_id
 AND svf.schema_id = sv.schema_id
 AND svf.version = sv.version
WHERE s.namespace_id = $1
  AND COALESCE(s.staged_version, s.current_version) IS NOT NULL
  AND s.deleted_at IS NULL
ORDER BY sv.schema_id, svf.filename;
