-- Append-only audit log for namespace re-parenting events. See
-- docs/design/namespace-hierarchy.md decision D9.
--
-- Phase 1 ships the schema and queries; callers (the SetNamespaceParent
-- RPC) wire them up in phase 3.

-- name: RecordNamespaceParentEvent :exec
INSERT INTO namespace_parent_events (
    namespace_id, previous_parent_id, new_parent_id, actor_id
)
VALUES ($1, $2, $3, $4);

-- ListNamespaceParentEvents returns the re-parenting history for a
-- namespace, newest first. Bounded by limit; for full history with
-- pagination, use the keyset variant below.
-- name: ListNamespaceParentEvents :many
SELECT id, namespace_id, previous_parent_id, new_parent_id, actor_id, occurred_at
FROM namespace_parent_events
WHERE namespace_id = $1
ORDER BY occurred_at DESC, id DESC
LIMIT $2;

-- ListNamespaceParentEventsPage returns events with id strictly less than
-- @after_id (or all events when @after_id is 0), newest first. Keyset
-- pagination on the BIGSERIAL primary key — stable under concurrent inserts.
-- name: ListNamespaceParentEventsPage :many
SELECT id, namespace_id, previous_parent_id, new_parent_id, actor_id, occurred_at
FROM namespace_parent_events
WHERE namespace_id = @namespace_id::text
  AND (@after_id::bigint = 0 OR id < @after_id::bigint)
ORDER BY id DESC
LIMIT @page_limit::int;
