-- Goals and goal_updates (P3.5, docs/11-goals-spec.md). This file is ASCII-only
-- and every UPDATE spells its RETURNING column list out rather than using *,
-- matching items.sql: sqlc's SQLite query rewriter has a byte-offset bug that a
-- single non-ASCII character earlier in the file is enough to trigger.
--
-- There is deliberately no query here for item-linked progress or velocity.
-- Both are a count over the chains view, which sqlc's analyzer cannot parse and
-- which sqlc.yaml therefore omits from the schema set. That count is
-- hand-written database/sql in internal/store/goals.go, the same exception
-- chains.go already is.

-- name: CreateGoal :one
-- CreateGoal writes a goal. Status is always 'active' at creation.
INSERT INTO goals (
  id, title, period_kind, period_start, period_end,
  item_id, target_count, status, created_at, updated_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING id, title, period_kind, period_start, period_end,
    item_id, target_count, status, created_at, updated_at;

-- name: GetGoal :one
SELECT id, title, period_kind, period_start, period_end,
    item_id, target_count, status, created_at, updated_at
FROM goals
WHERE id = ?;

-- ListActiveGoals is the set context injection and the briefing both read, so
-- it is ordered deterministically rather than left to the planner.
--
-- name: ListActiveGoals :many
SELECT id, title, period_kind, period_start, period_end,
    item_id, target_count, status, created_at, updated_at
FROM goals
WHERE status = 'active'
ORDER BY created_at, id;

-- name: ListAllGoals :many
SELECT id, title, period_kind, period_start, period_end,
    item_id, target_count, status, created_at, updated_at
FROM goals
ORDER BY created_at, id;

-- ListGoalsPastPeriod backs the sweeper's period-end evaluation. period_end is
-- an inclusive local ISO date, so a goal is due once the local date has moved
-- past it. status = 'active' is the whole idempotency guard: a goal this pass
-- evaluates is written terminal in the same pass and no longer matches, and the
-- transition is one-way, so an hourly re-run is a no-op (same shape as K6 for
-- missed on occurrences).
--
-- name: ListGoalsPastPeriod :many
SELECT id, title, period_kind, period_start, period_end,
    item_id, target_count, status, created_at, updated_at
FROM goals
WHERE status = 'active' AND period_end < ?
ORDER BY period_end, id;

-- UpdateGoal applies an update_goal edit. The terminal-goal guard is in Go
-- (Layer 2), so this is only ever reached for an active goal; status is in the
-- SET list for the one user-driven terminal transition, abandoned.
--
-- name: UpdateGoal :one
UPDATE goals
SET title = ?, period_start = ?, period_end = ?, target_count = ?,
    status = ?, updated_at = ?
WHERE id = ?
RETURNING id, title, period_kind, period_start, period_end,
    item_id, target_count, status, created_at, updated_at;

-- SetGoalStatus is the sweeper's guarded one-way write. The AND status =
-- 'active' clause makes a redundant hourly pass a benign zero-row update and
-- absorbs a race with a concurrent abandoned from update_goal.
--
-- name: SetGoalStatus :execrows
UPDATE goals
SET status = ?, updated_at = ?
WHERE id = ? AND status = 'active';

-- name: CreateGoalUpdate :one
INSERT INTO goal_updates (
  id, goal_id, note, progress_pct, source, created_at
) VALUES (
  ?, ?, ?, ?, ?, ?
)
RETURNING id, goal_id, note, progress_pct, source, created_at;

-- LatestGoalUpdate is a freestanding goal's current progress: a read of the
-- newest row, never a stored column, so it cannot drift from the log that
-- produced it. ErrNotFound means the goal has had no update at all, which the
-- period-end pass reads as missed.
--
-- name: LatestGoalUpdate :one
SELECT id, goal_id, note, progress_pct, source, created_at
FROM goal_updates
WHERE goal_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;
