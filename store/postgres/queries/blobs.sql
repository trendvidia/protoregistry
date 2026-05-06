-- name: PutBlob :exec
INSERT INTO proto_blobs (namespace_id, sha256, original_source, size_bytes)
VALUES ($1, $2, $3, $4)
ON CONFLICT (namespace_id, sha256) DO NOTHING;

-- name: GetBlob :one
SELECT namespace_id, sha256, original_source, size_bytes, created_at
FROM proto_blobs
WHERE namespace_id = $1 AND sha256 = $2;

-- name: GetBlobsByHashes :many
SELECT namespace_id, sha256, original_source, size_bytes, created_at
FROM proto_blobs
WHERE namespace_id = $1 AND sha256 = ANY($2::text[]);
