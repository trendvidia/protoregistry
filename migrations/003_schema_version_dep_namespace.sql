-- +goose Up

-- Adds the dep_namespace_id column to schema_version_deps to support
-- cross-namespace dependency pinning (decision D3 in
-- docs/design/namespace-hierarchy.md). Same-namespace deps (the only
-- case before this migration) are backfilled to dep_namespace_id =
-- namespace_id, then the column is made NOT NULL and added to the
-- primary key.
--
-- After this migration, a row in schema_version_deps means: "child
-- (namespace_id, schema_id, version) depends on file dep_filename from
-- schema dep_schema_id at version dep_version, contributed by namespace
-- dep_namespace_id." For same-namespace deps, dep_namespace_id ==
-- namespace_id. For cross-namespace deps (parent chain), dep_namespace_id
-- identifies the ancestor that supplied the file.

ALTER TABLE schema_version_deps
    ADD COLUMN dep_namespace_id TEXT REFERENCES namespaces(id);

UPDATE schema_version_deps SET dep_namespace_id = namespace_id;

ALTER TABLE schema_version_deps
    ALTER COLUMN dep_namespace_id SET NOT NULL;

ALTER TABLE schema_version_deps DROP CONSTRAINT schema_version_deps_pkey;
ALTER TABLE schema_version_deps
    ADD PRIMARY KEY (namespace_id, schema_id, version, dep_namespace_id, dep_schema_id, dep_filename);

-- Reverse-lookup index from a dep onto the children that pin it, useful
-- for "who depends on this parent file at this version?" queries (needed
-- by phase 2b's Restore and phase 4's Rebase).
CREATE INDEX idx_version_deps_dep_target
    ON schema_version_deps (dep_namespace_id, dep_schema_id, dep_filename, dep_version);

-- +goose Down

DROP INDEX IF EXISTS idx_version_deps_dep_target;
ALTER TABLE schema_version_deps DROP CONSTRAINT schema_version_deps_pkey;
ALTER TABLE schema_version_deps
    ADD PRIMARY KEY (namespace_id, schema_id, version, dep_schema_id, dep_filename);
ALTER TABLE schema_version_deps DROP COLUMN IF EXISTS dep_namespace_id;
