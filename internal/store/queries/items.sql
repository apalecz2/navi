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
