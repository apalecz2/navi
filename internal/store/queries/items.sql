-- name: CreateItem :one
INSERT INTO items (
  id, kind, title, notes, schedule, tz, tz_mode, notify_policy, priority,
  grace_period_minutes, reconcile_at, snooze_cap, attrs, created_at, updated_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetItem :one
SELECT * FROM items
WHERE id = ?;

-- ListActiveItems is the set injected into every agent turn, so it is ordered
-- deterministically rather than left to the planner.
--
-- name: ListActiveItems :many
SELECT * FROM items
WHERE active = 1 AND archived_at IS NULL
ORDER BY created_at, id;

-- ListUnarchivedItems backs list_items (P1). Filtering by active/paused
-- happens in Go, against domain.Item.IsPaused, rather than as a second
-- parameterized query - see sqlc.yaml's note on why sqlc.arg() is avoided
-- here.
--
-- Spells out its column list rather than SELECT * - see UpdateItem's comment
-- below for the sqlc bug this avoids.
--
-- name: ListUnarchivedItems :many
SELECT id, kind, title, notes, schedule, tz, tz_mode, notify_policy, priority,
    grace_period_minutes, reconcile_at, snooze_cap, active, paused_until, archived_at,
    attrs, source, external_id, etag, last_synced_at, created_at, updated_at
FROM items
WHERE archived_at IS NULL
ORDER BY created_at, id;

-- UpdateItem and ArchiveItem spell their RETURNING column list out rather
-- than using RETURNING *, and every comment in this block is plain ASCII on
-- purpose. sqlc's SQLite backend has a byte-offset bug in its query rewriter:
-- any non-ASCII character anywhere earlier in the file (an em dash in a
-- comment was the one found here) shifts the byte offsets it slices RETURNING
-- and SELECT * expansions at, so the emitted query text comes out spliced
-- with a fragment from a different position in the file - not a build error,
-- since the generated Go still compiles, only detectable by reading the
-- embedded SQL string or running the query. A third instance of the
-- query-mangling class sqlc.yaml already documents two of. Two independent
-- fixes are applied together here: spell out every "*" as an explicit column
-- list, and keep this file ASCII-only. Verified by bisection against a
-- minimal repro.
--
-- name: UpdateItem :one
UPDATE items
SET title = ?, notes = ?, schedule = ?, kind = ?, tz = ?, tz_mode = ?,
    notify_policy = ?, priority = ?, grace_period_minutes = ?, reconcile_at = ?,
    attrs = ?, updated_at = ?
WHERE id = ? AND archived_at IS NULL
RETURNING id, kind, title, notes, schedule, tz, tz_mode, notify_policy, priority,
    grace_period_minutes, reconcile_at, snooze_cap, active, paused_until, archived_at,
    attrs, source, external_id, etag, last_synced_at, created_at, updated_at;

-- ArchiveItem replaces deletion (A7): pending occurrences are cleared by the
-- caller's re-materialization, not by this statement, and resolved history is
-- untouched because it was never named here. See UpdateItem's comment above
-- for why RETURNING spells out its columns and this file stays ASCII-only.
--
-- name: ArchiveItem :one
UPDATE items
SET archived_at = ?, active = 0, updated_at = ?
WHERE id = ? AND archived_at IS NULL
RETURNING id, kind, title, notes, schedule, tz, tz_mode, notify_policy, priority,
    grace_period_minutes, reconcile_at, snooze_cap, active, paused_until, archived_at,
    attrs, source, external_id, etag, last_synced_at, created_at, updated_at;

-- PauseItem is the only statement in this file that writes paused_until (I6).
-- UpdateItem deliberately does not carry the column: pausing is a lifecycle
-- change like archiving, not a field edit, and it re-materializes for a
-- different reason - to clear the pending rows that now fall inside the
-- window. A NULL first parameter lifts the pause, which is the same statement
-- rather than a second one, because "away until Monday" is as revocable as it
-- is settable.
--
-- Pending occurrences inside the new window are deleted by the caller's
-- re-materialization, not here, exactly as ArchiveItem's are. See UpdateItem's
-- comment above for why RETURNING spells out its columns and this file stays
-- ASCII-only.
--
-- name: PauseItem :one
UPDATE items
SET paused_until = ?, updated_at = ?
WHERE id = ? AND archived_at IS NULL
RETURNING id, kind, title, notes, schedule, tz, tz_mode, notify_policy, priority,
    grace_period_minutes, reconcile_at, snooze_cap, active, paused_until, archived_at,
    attrs, source, external_id, etag, last_synced_at, created_at, updated_at;
