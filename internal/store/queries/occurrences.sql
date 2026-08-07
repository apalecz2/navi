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
