-- 0001_init: the schema in docs/04-data-model.md.
--
-- All timestamps are ISO-8601 UTC strings with a Z suffix, which sort and
-- compare correctly as text and avoid SQLite's lack of a native timestamp type.
-- Local wall-clock times within a schedule are HH:MM strings resolved against
-- the item's timezone at materialization. Identifiers are ULIDs stored as text.

-- The definition of a recurring thing. Never holds a timestamp for a specific
-- day; that is what occurrences are for (D-003).
CREATE TABLE items (
  id                    TEXT PRIMARY KEY,
  kind                  TEXT NOT NULL DEFAULT 'reminder'
                          CHECK (kind IN ('reminder', 'event')),
  title                 TEXT NOT NULL,
  notes                 TEXT,

  -- scheduling
  schedule              TEXT NOT NULL,             -- JSON, see 05-schedule-spec.md
  tz                    TEXT NOT NULL,             -- IANA, e.g. America/Toronto
  tz_mode               TEXT NOT NULL DEFAULT 'floating'
                          CHECK (tz_mode IN ('fixed', 'floating')),

  -- notification and resolution policy
  notify_policy         TEXT NOT NULL DEFAULT 'at_time'
                          CHECK (notify_policy IN ('at_time', 'silent', 'digest')),
  priority              INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
  grace_period_minutes  INTEGER,                   -- NULL = end of local day
  reconcile_at          TEXT,                      -- 'HH:MM' local; NULL = global default
  snooze_cap            INTEGER NOT NULL DEFAULT 3,

  -- lifecycle
  active                INTEGER NOT NULL DEFAULT 1,
  paused_until          TEXT,
  archived_at           TEXT,

  -- extensibility
  attrs                 TEXT NOT NULL DEFAULT '{}', -- JSON: location, attendees, etc.

  -- calendar sync (unused until X3)
  source                TEXT NOT NULL DEFAULT 'local'
                          CHECK (source IN ('local', 'google', 'apple')),
  external_id           TEXT,
  etag                  TEXT,
  last_synced_at        TEXT,

  created_at            TEXT NOT NULL,
  updated_at            TEXT NOT NULL
);

-- One materialized instance. This is the row everything operates on.
--
-- status carries no CHECK constraint on purpose: the valid set depends on
-- items.kind and the legal transitions between values are not expressible in a
-- column constraint, so the whole rule lives in internal/domain rather than
-- half here and half there.
CREATE TABLE occurrences (
  id                    TEXT PRIMARY KEY,
  item_id               TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,

  starts_at             TEXT NOT NULL,             -- ISO-8601 UTC
  ends_at               TEXT,                      -- NULL for instants (all reminders)

  status                TEXT NOT NULL DEFAULT 'pending',

  -- override and snooze chain
  is_override           INTEGER NOT NULL DEFAULT 0,
  parent_occurrence_id  TEXT REFERENCES occurrences(id),
  snooze_depth          INTEGER NOT NULL DEFAULT 0,

  -- lifecycle timestamps
  notified_at           TEXT,
  reconciled_at         TEXT,
  resolved_at           TEXT,

  -- resolution detail
  resolution_note       TEXT,                      -- e.g. "on vacation"
  resolution_source     TEXT CHECK (resolution_source IN
                          ('notification', 'web', 'agent', 'sweeper')),

  -- generated message
  message_text          TEXT,
  message_model         TEXT,
  message_generated_at  TEXT,
  generation_attempts   INTEGER NOT NULL DEFAULT 0,
  generation_pass       INTEGER NOT NULL DEFAULT 0, -- 0 none, 1 safety net, 2 refreshed

  created_at            TEXT NOT NULL
);

-- Agent message history. Capped by query, not by deletion, since it is small
-- and useful.
CREATE TABLE conversations (
  id            TEXT PRIMARY KEY,
  role          TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
  content       TEXT NOT NULL,
  tool_calls    TEXT,                    -- JSON
  tool_call_id  TEXT,
  transport     TEXT,
  external_id   TEXT,
  context_ref   TEXT,                    -- e.g. 'reconcile:2026-08-05'
  created_at    TEXT NOT NULL
);

-- Every model call. This is the table that makes the tiering worth having,
-- because it turns escalation tuning into a query rather than a guess.
CREATE TABLE llm_calls (
  id                 TEXT PRIMARY KEY,
  task               TEXT NOT NULL,       -- 'crud' | 'copywriter' | 'digest' | 'reconcile'
  tier               INTEGER NOT NULL,
  model              TEXT NOT NULL,
  prompt_tokens      INTEGER,
  completion_tokens  INTEGER,
  latency_ms         INTEGER,
  escalated          INTEGER NOT NULL DEFAULT 0,
  escalation_reason  TEXT,
  error              TEXT,
  occurrence_id      TEXT,                -- when the call was for a specific occurrence
  created_at         TEXT NOT NULL
);

-- Small pieces of singleton state that do not justify a table: current_tz,
-- last_materialized_through, last_reconcile_date, proactive_count:{date},
-- global_pause_until, and the applied schema version.
CREATE TABLE kv (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

-- Partial indexes matter more than usual here. status = 'pending' is a small
-- slice of a table that grows forever, and the scheduler runs that query every
-- 30 seconds for the life of the system.

-- scheduler hot path: due and pending
CREATE INDEX idx_occ_due ON occurrences(starts_at) WHERE status = 'pending';

-- copywriter: needs text, due soon
CREATE INDEX idx_occ_ungenerated ON occurrences(starts_at)
  WHERE status = 'pending' AND message_text IS NULL;

-- reconciler and day view: everything for a date
CREATE INDEX idx_occ_status_starts ON occurrences(status, starts_at);

-- calendar range query and per-item history
CREATE INDEX idx_occ_item_starts ON occurrences(item_id, starts_at);

-- chain walking
CREATE INDEX idx_occ_parent ON occurrences(parent_occurrence_id)
  WHERE parent_occurrence_id IS NOT NULL;

-- active items, the set injected into every agent turn
CREATE INDEX idx_items_active ON items(active) WHERE archived_at IS NULL;

CREATE INDEX idx_conv_created ON conversations(created_at DESC);

CREATE INDEX idx_llm_created ON llm_calls(created_at DESC);
