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

-- ResolveOccurrence is POST /api/occurrences/{id}/resolve's write half, run
-- only after domain.Transition has already answered OutcomeApplied. It writes
-- all four resolution columns together because they are one fact - a row
-- carrying a status but no resolved_at is not a resolution anyone can read.
--
-- The trailing status = ? guard mirrors ClaimOccurrence's and
-- ReleaseClaimedOccurrence's. The single BEGIN IMMEDIATE writer already makes
-- the read above it and this write atomic, so the guard is redundant defence
-- rather than the mechanism, and it is kept for the same reason every other
-- guarded write in this file keeps one: a planner bug cannot reach a row the
-- statement itself refuses.
--
-- name: ResolveOccurrence :execrows
UPDATE occurrences
SET status = ?, resolved_at = ?, resolution_note = ?, resolution_source = ?
WHERE id = ?
  AND status = ?;

-- ChildOccurrence is the row a snooze wrote for its parent - the live link of
-- a chain, one step down.
--
-- It is :one because a snoozed row has exactly one child by construction: the
-- second snooze of the same row is "already in the requested terminal state",
-- which domain.Transition answers OutcomeNoop and which writes nothing. So a
-- double-tapped Snooze button reads the child that already exists rather than
-- minting a second one. idx_occ_parent covers the lookup.
--
-- The column list is spelled out rather than *, and this comment is plain
-- ASCII, matching OverrideFuturePendingOccurrence above - see items.sql's
-- UpdateItem for the sqlc truncation bug both avoid.
--
-- name: ChildOccurrence :one
SELECT id, item_id, starts_at, ends_at, status, is_override, parent_occurrence_id,
    snooze_depth, notified_at, reconciled_at, resolved_at, resolution_note,
    resolution_source, message_text, message_model, message_generated_at,
    generation_attempts, generation_pass, created_at
FROM occurrences
WHERE parent_occurrence_id = ?;

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

-- ListUnreconciled is what the daily check-in asks about (K5): everything
-- still unresolved for the local day and never yet asked about, across every
-- item. The bounds are the day's local midnight and now, resolved in the
-- device zone, so an occurrence later today is future rather than outstanding.
--
-- Deliberately no notify_policy filter, which is the one difference from
-- ListDueOccurrences above. K5 wants silent items - which never pushed and
-- would otherwise be invisible - and at_time items that were notified and
-- ignored, in the same message; naming the policies here would be a list to
-- forget to update when Q-5 decides digest's fate.
--
-- reconciled_at IS NULL is what makes a row asked about at most once. It is a
-- per-row fact rather than a per-pass one, so the 21:00 pass does not re-ask
-- about an item whose own 14:00 override already covered it, and next
-- session's grace window has the instant it measures from (K6, D-008).
--
-- The last predicate is K8: ifnull(reconcile_at, <global>) <= <this pass>.
-- Both binds are zero-padded local HH:MM, so the string comparison is the
-- chronological one. A pass therefore covers every item due to be asked at or
-- before it, which is what lets the evening pass sweep an afternoon item's
-- later occurrence. The cast is there for sqlc rather than for SQLite: without
-- it the analyzer cannot infer a type for a bare placeholder inside ifnull and
-- emits an interface{} parameter, which is a hole in exactly the typing this
-- whole arrangement exists to get.
--
-- name: ListUnreconciled :many
SELECT o.id, o.item_id, o.starts_at, o.status, i.title
FROM occurrences o
JOIN items i ON i.id = o.item_id
WHERE o.status IN ('pending', 'notified')
  AND o.reconciled_at IS NULL
  AND o.starts_at >= ?
  AND o.starts_at <= ?
  AND i.kind = 'reminder'
  AND i.active = 1
  AND i.archived_at IS NULL
  AND ifnull(i.paused_until, '0000-01-01T00:00:00Z') <= ?
  AND ifnull(i.reconcile_at, cast(? as text)) <= ?
ORDER BY o.starts_at, o.id;

-- MarkReconciled records that the check-in asked about this row. It is not a
-- status transition and must never be one: the occurrence is still pending or
-- notified, and what it is waiting for is an answer. The reconciled_at IS NULL
-- guard makes a re-run write nothing rather than move the instant grace is
-- measured from, the same defence every other guarded write in this file
-- carries.
--
-- name: MarkReconciled :execrows
UPDATE occurrences
SET reconciled_at = ?
WHERE id = ?
  AND reconciled_at IS NULL;
