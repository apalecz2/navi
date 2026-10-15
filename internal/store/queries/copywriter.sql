-- The copywriter's four statements (docs/06-agent-spec.md#copywriter).
--
-- Every write is guarded with status = 'pending'. The scheduler claims a row
-- by moving it to notified, and a generation that finishes after that must be a
-- zero-row update and never a rewrite of something already sent - the body a
-- notification carries is read inside the claim's own transaction, so a late
-- write could not change what went out, but it would still scribble on history
-- (invariant 2). The guard is in the SQL and not in the caller for the reason
-- every guarded write in this directory gives.
--
-- name: ListCopyCandidates :many
SELECT o.id, o.item_id, o.starts_at, o.snooze_depth, o.parent_occurrence_id,
       o.generation_pass, o.generation_attempts, o.message_basis
FROM occurrences o
JOIN items i ON i.id = o.item_id
WHERE o.status = 'pending'
  AND o.starts_at > ?
  AND o.starts_at <= ?
  AND o.generation_attempts < ?
  AND ((o.generation_pass = 0 AND o.message_text IS NULL)
       OR (o.generation_pass = 1 AND o.starts_at <= ?))
  AND i.kind = 'reminder'
  AND i.notify_policy = 'at_time'
  AND i.active = 1
  AND i.archived_at IS NULL
  AND ifnull(i.paused_until, '0000-01-01T00:00:00Z') <= ?
ORDER BY o.starts_at, o.id
LIMIT ?;

-- ClaimGenerationAttempt is recorded before the model is called, so a crash
-- mid-call still costs an attempt and the cap holds. generation_pass = ? names
-- the pass the row is currently at (0 for the safety net, 1 for the refresh),
-- which is what makes the counter per pass: a successful write resets it.
--
-- name: ClaimGenerationAttempt :execrows
UPDATE occurrences
SET generation_attempts = generation_attempts + 1
WHERE id = ?
  AND status = 'pending'
  AND generation_pass = ?
  AND generation_attempts < ?;

-- name: WriteGeneratedMessage :execrows
UPDATE occurrences
SET message_text = ?, message_model = ?, message_generated_at = ?,
    generation_pass = ?, generation_attempts = 0, message_basis = ?
WHERE id = ?
  AND status = 'pending'
  AND generation_pass = ?;

-- name: ListRecentMessages :many
SELECT message_text
FROM occurrences
WHERE item_id = ?
  AND id != ?
  AND message_text IS NOT NULL
  AND message_generated_at IS NOT NULL
ORDER BY message_generated_at DESC, id DESC
LIMIT ?;
