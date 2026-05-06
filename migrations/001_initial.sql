-- +goose Up

CREATE TABLE namespaces (
    id          TEXT        PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ,
    metadata    JSONB       NOT NULL DEFAULT '{}'
);

CREATE TABLE proto_blobs (
    namespace_id    TEXT        NOT NULL REFERENCES namespaces(id),
    sha256          TEXT        NOT NULL,
    original_source BYTEA       NOT NULL,
    size_bytes      INTEGER     NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, sha256)
);

CREATE TABLE schemas (
    namespace_id    TEXT        NOT NULL REFERENCES namespaces(id),
    schema_id       TEXT        NOT NULL,
    current_version BIGINT,
    staged_version  BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    metadata        JSONB       NOT NULL DEFAULT '{}',
    PRIMARY KEY (namespace_id, schema_id)
);

CREATE TABLE schema_versions (
    namespace_id     TEXT        NOT NULL,
    schema_id        TEXT        NOT NULL,
    version          BIGINT      NOT NULL,
    compiled         BYTEA       NOT NULL,
    compiler_version TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by       TEXT        NOT NULL DEFAULT '',
    deleted_at       TIMESTAMPTZ,
    metadata         JSONB       NOT NULL DEFAULT '{}',
    PRIMARY KEY (namespace_id, schema_id, version),
    FOREIGN KEY (namespace_id, schema_id) REFERENCES schemas(namespace_id, schema_id)
);

CREATE TABLE schema_version_files (
    namespace_id TEXT    NOT NULL,
    schema_id    TEXT    NOT NULL,
    version      BIGINT  NOT NULL,
    filename     TEXT    NOT NULL,
    blob_sha256  TEXT    NOT NULL,
    PRIMARY KEY (namespace_id, schema_id, version, filename),
    FOREIGN KEY (namespace_id, schema_id, version)
        REFERENCES schema_versions(namespace_id, schema_id, version),
    FOREIGN KEY (namespace_id, blob_sha256)
        REFERENCES proto_blobs(namespace_id, sha256)
);

CREATE TABLE schema_version_deps (
    namespace_id  TEXT   NOT NULL,
    schema_id     TEXT   NOT NULL,
    version       BIGINT NOT NULL,
    dep_schema_id TEXT   NOT NULL,
    dep_filename  TEXT   NOT NULL,
    dep_version   BIGINT NOT NULL,
    PRIMARY KEY (namespace_id, schema_id, version, dep_schema_id, dep_filename),
    FOREIGN KEY (namespace_id, schema_id, version)
        REFERENCES schema_versions(namespace_id, schema_id, version)
);

CREATE INDEX idx_schemas_current
    ON schemas (namespace_id, schema_id, current_version)
    WHERE current_version IS NOT NULL;

CREATE INDEX idx_schemas_staged
    ON schemas (namespace_id, schema_id, staged_version)
    WHERE staged_version IS NOT NULL;

CREATE INDEX idx_version_deps_reverse
    ON schema_version_deps (namespace_id, dep_schema_id);

CREATE INDEX idx_version_files_blob
    ON schema_version_files (blob_sha256);

-- +goose Down

DROP INDEX IF EXISTS idx_version_files_blob;
DROP INDEX IF EXISTS idx_version_deps_reverse;
DROP INDEX IF EXISTS idx_schemas_staged;
DROP INDEX IF EXISTS idx_schemas_current;
DROP TABLE IF EXISTS schema_version_deps;
DROP TABLE IF EXISTS schema_version_files;
DROP TABLE IF EXISTS schema_versions;
DROP TABLE IF EXISTS schemas;
DROP TABLE IF EXISTS proto_blobs;
DROP TABLE IF EXISTS namespaces;
