# Data Model

SQLite, WAL mode. All timestamps are ISO-8601 UTC strings with a `Z` suffix, which
sort and compare correctly as text and avoid SQLite's lack of a native timestamp
type. Local wall-clock times within a schedule are stored as `HH:MM` strings and
resolved against the item's timezone at materialization.

Identifiers are ULIDs stored as text (`oklog/ulid`), so they sort chronologically
and are safe to put in URLs.

The DDL below is the source of truth. **sqlc** compiles it, plus the hand-written
queries, into typed Go structs and methods — there is no ORM and no model layer
that could drift from the schema. That direction of dependency is deliberate: the
partial indexes, the recursive-CTE `chains` view, and the `BEGIN IMMEDIATE` claim
are the load-bearing parts of this design, and they are precisely what an ORM
either abstracts badly or cannot express.

## Tables

### `items`

The definition of a recurring thing. Never holds a timestamp for a specific day.

```sql
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
```

`attrs` exists so that adding event fields later does not mean twelve nullable
columns that are always null for reminders. Anything kind-specific goes there.

`archived_at` rather than a hard delete, because resolved occurrences reference
the item and the statistics need its title. Deleting an item sets `archived_at`
and removes its `pending` occurrences.

### `occurrences`

One materialized instance. This is the row everything operates on.

```sql
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
```

`is_override` is load-bearing. Without it, a single-occurrence edit ("skip
tomorrow's") is silently undone by the next materialization run. The materializer
must never delete or overwrite a row where `is_override = 1`.

`resolution_source` is worth having: after a month it tells you whether you
actually resolve from notifications, the web app, or by messaging, which should
inform where effort goes next.

### `conversations`

Agent message history. Capped by query, not by deletion, since it is small and
useful.

```sql
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
```

`context_ref` lets a reply be recognised as an answer to the reconciliation
message rather than a new request, which is what makes "stretching and vitamins
yes, skipped the walk" resolve correctly.

### `llm_calls`

Every model call. This is the table that makes the tiering worth having, because
it turns escalation tuning into a query rather than a guess.

```sql
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
```

Retention: 90 days. It will be the largest table by row count within a month and
nothing older is useful once the ladder is tuned.

### `kv`

Small pieces of singleton state that do not justify a table.

```sql
CREATE TABLE kv (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
```

Keys in use:

| Key | Purpose |
|---|---|
| `current_tz` | Device timezone, drives `tz_mode = 'floating'` items |
| `last_materialized_through` | Horizon date, so the sweeper can detect a missed run |
| `last_reconcile_date` | Prevents a duplicate check-in after a restart |
| `proactive_count:{date}` | Daily cap on unprompted agent messages |
| `global_pause_until` | Vacation mode |

Every key in that table except `last_materialized_through` is *per-person* state
stored as a singleton, which is correct under S1 and is the assumption that would
have to move first if S1 ever did. Noted here rather than designed around: the fix
is a composite primary key, not a different table.

## Status state machines

Kept in one module and enforced by every surface. This is what makes idempotency
a property of the design rather than three separate bolt-ons.

### Reminders

```
                    ┌────────────────────────────────┐
                    │                                │
  pending ────────▶ notified ────────▶ completed     │
     │  (scheduler)     │                  ▲         │
     │                  │                  │         │
     │                  ├──▶ skipped       │         │
     │                  │                  │         │
     │                  ├──▶ snoozed ──────┘         │
     │                  │      (child occurrence     │
     │                  │       resolves the chain)  │
     │                  │                            │
     │                  └──▶ missed ◀────────────────┘
     │                        (reconciler, after grace)
     │
     ├──────────────▶ completed     (resolved early, US-4.1)
     ├──────────────▶ skipped       (resolved early)
     └──────────────▶ missed        (reconciler, silent items)
```

Terminal states: `completed`, `skipped`, `missed`. `snoozed` is terminal for that
row but the chain continues in the child.

Transition rules:

| From | To | Trigger | Notes |
|---|---|---|---|
| `pending` | `notified` | scheduler | Only when `notify_policy = 'at_time'` |
| `pending` | `completed` / `skipped` | agent, web | Early resolution; cancels the pending notification |
| `pending` | `missed` | reconciler | Silent items unresolved past grace |
| `notified` | `completed` / `skipped` | any surface | |
| `notified` | `snoozed` | any surface | Creates child; rejected at `snooze_depth >= snooze_cap` |
| `notified` | `missed` | reconciler | Asked, no answer, grace elapsed |
| any terminal | same terminal | any surface | Idempotent no-op, returns 200 with current state |
| any terminal | different terminal | any surface | Rejected, 409 |

The last two rows are the whole idempotency story. A double-tapped notification
button and a flaky mobile retry both land on "same terminal state," which is a
no-op.

### Events (later)

```
  pending ──▶ occurred
     └──────▶ cancelled
```

No completion semantics. Same table, different valid set, enforced by looking at
`items.kind`.

## Streaks and snooze chains

Statistics operate on **chains**, not rows. A chain is an occurrence plus its
snooze descendants, identified by walking `parent_occurrence_id` to the root.

- A chain is `completed` if any link is `completed`.
- A chain is `skipped` if the terminal link is `skipped`.
- A chain is `missed` if the terminal link is `missed`, including snooze-cap
  exhaustion.
- `snooze_count` for a chain is the maximum `snooze_depth` in it.

This is why snoozing is implemented as a child row rather than by mutating
`starts_at`. If a snooze broke a streak, the rational response is to ignore the
notification instead of snoozing, which produces no data at all. The chain rule
makes honest snoozing free.

A convenience view keeps the aggregation queries readable:

```sql
CREATE VIEW chains AS
WITH RECURSIVE walk(root_id, id) AS (
  SELECT id, id FROM occurrences WHERE parent_occurrence_id IS NULL
  UNION ALL
  SELECT w.root_id, o.id
    FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id
)
SELECT
  w.root_id,
  r.item_id,
  r.starts_at                                        AS scheduled_at,
  MAX(o.snooze_depth)                                AS snooze_count,
  MAX(o.status = 'completed')                        AS was_completed,
  MIN(CASE WHEN o.status = 'completed' THEN o.resolved_at END) AS completed_at,
  r.notified_at
FROM walk w
JOIN occurrences o ON o.id = w.id
JOIN occurrences r ON r.id = w.root_id
GROUP BY w.root_id;
```

Both the dashboard and the agent's `get_stats` tool query this view, satisfying
V6 by construction rather than by discipline.

## Indexes

```sql
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
```

Partial indexes matter more than usual here. `status = 'pending'` is a small
slice of a table that grows forever, and the scheduler runs that query every 30
seconds for the life of the system.

## Retention

| Table | Policy |
|---|---|
| `items` | Never deleted, archived instead |
| `occurrences` | Never deleted once resolved. This is the statistics dataset. |
| `conversations` | 180 days |
| `llm_calls` | 90 days |
| `kv` | Manual |

The hourly sweeper enforces these. Occurrence retention being unbounded is fine:
a dozen items firing daily produces roughly four thousand rows a year, which
SQLite does not notice.

## Migrations

Plain numbered SQL files applied in order, with the applied version in `kv`. No
migration framework. At this scale the framework is more machinery than the
problem justifies, and a single-user database can afford a restore-from-R2 if a
migration goes wrong.

The files are embedded with `//go:embed` and applied on startup, which keeps D11
intact — a single binary with no migration step to forget and no external tool to
install on the host.
