-- +goose Up

-- Adds the namespace hierarchy column and the re-parenting audit table.
-- See docs/design/namespace-hierarchy.md for the design.
--
-- Strictly additive: existing rows get parent_namespace_id = NULL, which
-- is the implicit root (resolution falls back to __builtins__, then to
-- Google WKT). Behavior is bit-identical to today until later phases
-- wire chain resolution into the compiler.

ALTER TABLE namespaces
    ADD COLUMN parent_namespace_id TEXT REFERENCES namespaces(id);

-- Audit log for re-parenting events (decision D9). Append-only; rows
-- are never updated or deleted. Populated in the same transaction as
-- the parent update.
CREATE TABLE namespace_parent_events (
    id                   BIGSERIAL    PRIMARY KEY,
    namespace_id         TEXT         NOT NULL REFERENCES namespaces(id),
    previous_parent_id   TEXT,
    new_parent_id        TEXT,
    actor_id             TEXT         NOT NULL,
    occurred_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_namespace_parent_events_namespace
    ON namespace_parent_events (namespace_id, occurred_at DESC);

CREATE INDEX idx_namespaces_parent
    ON namespaces (parent_namespace_id)
    WHERE parent_namespace_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_namespaces_parent;
DROP INDEX IF EXISTS idx_namespace_parent_events_namespace;
DROP TABLE IF EXISTS namespace_parent_events;
ALTER TABLE namespaces DROP COLUMN IF EXISTS parent_namespace_id;
