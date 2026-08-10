-- name: CreateOccurrence :one
INSERT INTO occurrences (
  id, item_id, starts_at, ends_at, status, is_override,
  parent_occurrence_id, snooze_depth, message_text, created_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetOccurrence :one
SELECT * FROM occurrences
WHERE id = ?;

-- name: ListOccurrencesForItem :many
SELECT * FROM occurrences
WHERE item_id = ?
ORDER BY starts_at, id;

-- name: ListFutureOccurrencesForItem :many
SELECT * FROM occurrences
WHERE item_id = ?
  AND starts_at > ?
ORDER BY starts_at, id;

-- name: DeleteFuturePendingOccurrence :execrows
DELETE FROM occurrences
WHERE id = ?
  AND item_id = ?
  AND status = 'pending'
  AND is_override = 0
  AND starts_at > ?;

-- OverrideFuturePendingOccurrence is update_item's scope=single mechanism
-- (05-schedule-spec.md#edit-scope): retime exactly one occurrence and mark it
-- is_override, so the materializer's plan/delete cycle leaves it alone from
-- then on. The guard mirrors DeleteFuturePendingOccurrence's exactly - only a
-- pending, non-override, still-future row can be touched this way.
--
-- RETURNING spells out its column list rather than using *, and this comment
-- is plain ASCII, matching items.sql's UpdateItem/ArchiveItem - see the
-- comment there for the sqlc bug this avoids.
--
-- name: OverrideFuturePendingOccurrence :one
UPDATE occurrences
SET starts_at = ?, is_override = 1
WHERE id = ?
  AND item_id = ?
  AND status = 'pending'
  AND is_override = 0
  AND starts_at > ?
RETURNING id, item_id, starts_at, ends_at, status, is_override, parent_occurrence_id,
    snooze_depth, notified_at, reconciled_at, resolved_at, resolution_note,
    resolution_source, message_text, message_model, message_generated_at,
    generation_attempts, generation_pass, created_at;

-- name: CountPendingOverdue :one
SELECT count(*) FROM occurrences o
JOIN items i ON i.id = o.item_id
WHERE o.status = 'pending'
  AND o.starts_at <= ?
  AND o.starts_at >= ?
  AND i.kind = 'reminder'
  AND i.notify_policy = 'at_time'
  AND i.active = 1
  AND i.archived_at IS NULL
  AND ifnull(i.paused_until, '0000-01-01T00:00:00Z') <= ?;

-- name: ListDueOccurrences :many
SELECT o.id, o.item_id, o.starts_at, o.message_text,
       i.title, i.kind, i.priority
FROM occurrences o
JOIN items i ON i.id = o.item_id
WHERE o.status = 'pending'
  AND o.starts_at <= ?
  AND o.starts_at >= ?
  AND i.kind = 'reminder'
  AND i.notify_policy = 'at_time'
  AND i.active = 1
  AND i.archived_at IS NULL
  AND ifnull(i.paused_until, '0000-01-01T00:00:00Z') <= ?
ORDER BY o.starts_at, o.id
LIMIT ?;

-- name: ClaimOccurrence :execrows
UPDATE occurrences
SET status = 'notified', notified_at = ?
WHERE id = ?
  AND status = 'pending';

-- name: ReleaseClaimedOccurrence :execrows
UPDATE occurrences
SET status = 'pending', notified_at = NULL
WHERE id = ?
  AND status = 'notified'
  AND notified_at = ?;

-- ListOccurrencesInRange is the agent's "today's occurrences" context block
-- (docs/06-agent-spec.md#context-injection): every occurrence, across every
-- item, starting inside [?, ?). Callers pass the device timezone's local-day
-- bounds converted to instants, so "today" here always means the same day
-- the rest of the injected context does. archived_at IS NULL excludes stale
-- history left behind by a deleted item.
--
-- name: ListOccurrencesInRange :many
SELECT o.id, o.item_id, o.starts_at, o.status, o.resolved_at, o.resolution_source,
       i.title
FROM occurrences o
JOIN items i ON i.id = o.item_id
WHERE o.starts_at >= ?
  AND o.starts_at < ?
  AND i.archived_at IS NULL
ORDER BY o.starts_at, o.id;
