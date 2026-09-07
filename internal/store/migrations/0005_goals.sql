-- 0005_goals: goals and goal_updates, from docs/11-goals-spec.md#schema and
-- docs/04-data-model.md. P3.5's first migration.
--
-- A goal is a target over a period, not a recurring thing with a fire instant,
-- which is why it is its own table rather than a third items.kind (D-024). It
-- does not materialize, is never claimed by the scheduler, and does not go
-- through the occurrence status machine.
--
-- Two shapes, one table. Item-linked: target_count against an existing item's
-- chains over the period; progress is a query, never a stored counter.
-- Freestanding: no item, progress self-reported append-only into goal_updates,
-- the same pattern conversations uses instead of a single "last message" column.
--
-- target_count IS NULL XOR item_id IS NULL is deliberately NOT a CHECK here.
-- SQLite could express the null pair, but the clearer rejection belongs in Go
-- beside every other goal rule (internal/agent), so the message reads like the
-- schedule ones the escalation ladder already feeds back to a model. See
-- docs/11-goals-spec.md#validation.
--
-- This file adds tables and no view, so it is listed in sqlc.yaml's schema set
-- (unlike 0002/0004, which recreate the recursive chains view sqlc cannot
-- parse).

CREATE TABLE goals (
  id            TEXT PRIMARY KEY,
  title         TEXT NOT NULL,
  period_kind   TEXT NOT NULL CHECK (period_kind IN ('day', 'week', 'month', 'custom')),
  period_start  TEXT NOT NULL,             -- ISO date, local
  period_end    TEXT NOT NULL,             -- ISO date, local, inclusive

  item_id       TEXT REFERENCES items(id), -- NULL for freestanding goals
  target_count  INTEGER,                   -- required when item_id is set

  status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'met', 'missed', 'abandoned')),

  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE goal_updates (
  id            TEXT PRIMARY KEY,
  goal_id       TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
  note          TEXT,                      -- freeform, e.g. "first draft done"
  progress_pct  INTEGER CHECK (progress_pct BETWEEN 0 AND 100),
  source        TEXT NOT NULL CHECK (source IN ('agent', 'web', 'briefing')),
  created_at    TEXT NOT NULL
);

-- active goals, the set the briefing (P3.5, next session) and context injection
-- both read.
CREATE INDEX idx_goals_active ON goals(status) WHERE status = 'active';

-- item-linked goals, walked when a completed occurrence might move a number.
CREATE INDEX idx_goals_item ON goals(item_id) WHERE item_id IS NOT NULL;

-- a freestanding goal's current progress is its newest row; the sweeper's
-- period-end pass reads the same.
CREATE INDEX idx_goal_updates_goal ON goal_updates(goal_id, created_at DESC);
