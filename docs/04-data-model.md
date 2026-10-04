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
                          ('notification', 'web', 'agent', 'sweeper', 'reconciler')),

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

`reconciler` was added in session 17 (migration `0004`), when the grace pass
became the first thing able to assign `missed` for the reason K6 gives it. It is
deliberately not `sweeper`: the sweeper resolves nothing, and a source that names
a loop which never wrote a resolution makes the column unreadable for exactly as
long as it takes to collect. It is also the only value no user-facing surface can
write — nobody reports a miss, the system concludes one.

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
  task               TEXT NOT NULL,       -- 'crud' | 'copywriter' | 'digest' | 'reconcile' | 'briefing' (P3.5)
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

### `goals` and `goal_updates`

**Status: specified, not built** — see [11-goals-spec.md](11-goals-spec.md),
scheduled for [P3.5](09-roadmap.md#p35-goals--briefing). A goal is a target
over a period, not a recurring thing with a fire instant, which is why it is
its own table rather than a third `items.kind` ([D-024](08-decisions.md#d-024-goals-are-a-first-class-entity-not-a-third-item-kind)).

```sql
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
  note          TEXT,
  progress_pct  INTEGER CHECK (progress_pct BETWEEN 0 AND 100),
  source        TEXT NOT NULL CHECK (source IN ('agent', 'web', 'briefing')),
  created_at    TEXT NOT NULL
);
```

An item-linked goal's progress is always a query over `chains`, never a stored
counter. A freestanding goal's progress is its most recent `goal_updates` row
— an append-only log standing in for a mutable current-value column, the same
pattern `conversations` already uses instead of a single "last message"
field. See [11-goals-spec.md](11-goals-spec.md#schema) for the validation
rules this schema is paired with.

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
| `awaiting_response:{date}` | Set when the morning briefing sends, cleared on reply; read by the grace-window pass (P3.5, 11-goals-spec.md) |
| `last_briefing_date` | Prevents a duplicate briefing after a restart, same role as `last_reconcile_date` (P3.5) |

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

A convenience view keeps the aggregation queries readable. This is the definition
as of migration `0007`, which changed two things over the original: it gained the
three columns statistics need to say how a chain *ended*, and it walks per root
instead of from every root, so a `WHERE` on it prunes before anything is walked.
(SQLite cannot push a predicate into a recursive CTE, so the original — one CTE
seeded with every parentless row, then grouped — filtered a finished result: a
one-week range cost what `all` cost, 170 ms at 20k occurrences and 1.9 s at 200k.
The per-root form answers a week in about half a millisecond at either size, and
`all` in 66 ms at 20k.)

```sql
CREATE VIEW chains AS
SELECT
  r.id                                   AS root_id,
  r.item_id,
  r.starts_at                            AS scheduled_at,
  (<walk> SELECT MAX(o.snooze_depth) ...)                          AS snooze_count,
  (<walk> SELECT MAX(o.status = 'completed') ...)                  AS was_completed,
  (<walk> SELECT MIN(CASE WHEN o.status = 'completed'
                          THEN o.resolved_at END) ...)             AS completed_at,
  r.notified_at,
  (<walk> SELECT MAX(o.reconciled_at IS NOT NULL) ...)             AS was_reconciled,
  (<walk> SELECT o.status ... ORDER BY o.snooze_depth DESC LIMIT 1) AS terminal_status,
  (<walk> SELECT o.resolution_note ... ORDER BY o.snooze_depth DESC LIMIT 1)
                                                                   AS terminal_note
FROM occurrences r
WHERE r.parent_occurrence_id IS NULL;

-- <walk> is, per column and correlated on r:
--   WITH RECURSIVE walk(id) AS (
--     SELECT r.id
--     UNION ALL
--     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
--   ... FROM walk w JOIN occurrences o ON o.id = w.id
```

The full text is in `internal/store/migrations/0007_chains_terminal.sql`. The walk is
spelled out once per column because SQLite has no lateral join to share it with;
change one and change all. Chains are one to four links.

`terminal_status` is the status of the last link (the one with the greatest
`snooze_depth`; a chain is linear because snooze is legal only from `notified` and
writes exactly one child). `was_reconciled` is "a check-in named some link". Neither
is read by anything except statistics.

Both the dashboard and the agent's `get_stats` tool query this view, through one
aggregation package (`internal/stats`), satisfying V6 by construction rather than by
discipline.

### Statistics definitions

These are the rest of what the rules above leave open. `internal/stats`'s package
doc carries the same text beside the code.

**Classes.** A chain is exactly one of: `completed` (any link completed — D-011),
`skipped` (not completed, last link skipped), `missed` (not completed, last link
missed), `awaiting` (not resolved, but `was_reconciled`: a check-in asked and
nothing has answered yet), or `open` (not resolved, nobody has asked: pending, or
notified and so far ignored).

**Completion rate** = `completed / (completed + missed)`. Skipped is out of both
sides: R2 makes it distinct from a miss, and counting it in the denominator would
punish the honest "I was away" it exists to record, while counting it as a success
would reward saying skip over doing the thing. Awaiting and open are out because
they are neither yet. No settled chains means no rate: `null`, never `0`.

**Streaks** are per item, over its chains in scheduled order up to now, and are
lifetime figures — they do not move with the requested range. A completed chain
adds one to the current run; a missed chain ends it; nothing else touches it. A
skip neither breaks nor extends the run, which waits across it. An awaiting chain,
including the most recent one still inside its grace window, and an open chain do
the same: only asked-and-got-nothing is a miss (K6), so only that ends a run. This
is also why V7 needs no code — a chain exists only for a day the schedule produced.

**Median lag** is minutes from a chain's first notification (the root's
`notified_at`) to its completion, over chains that were completed, were notified,
and completed no earlier than notified. A silent item has no `notified_at`; a row
resolved before it was ever sent was never claimed (R3). A snooze chain measures
from the first ask, so one pushed three times reads as slow — D-011's stated cost.
`null` with no samples.

**Windows.** `week`, `month`, `quarter` are trailing 7, 30 and 90 local calendar
days ending today, from local midnight to now, in the device zone
(`schedule.Zones.Local()`). A chain belongs to a window by its root's
`scheduled_at`; chains not yet due are not in it. `all` is everything up to now.
This is deliberately not the Monday calendar week goals use: a goal is a commitment
about a named period, while a statistic is a trailing measurement, and a calendar
week would be one data point on Monday morning and reset to empty every Monday.
Timeseries buckets are local dates or local Mondays, filed under the root's
`scheduled_at` ("how did what was due that day go" — not the calendar's rule, which
files a chain under its live link). The heatmap is the one exception: it buckets by
the local weekday and hour of the *completion*.

**Archived items** stay in aggregates, in `all`, and in per-item results, flagged
`archived`: history is immutable, and a rate that shrank when an old item was
retired would depend on housekeeping. Active items are always listed in a summary;
an archived item only when it has chains in the window.

## Indexes

```sql
-- scheduler hot path: due and pending
CREATE INDEX idx_occ_due ON occurrences(starts_at) WHERE status = 'pending';

-- copywriter: needs text, due soon
CREATE INDEX idx_occ_ungenerated ON occurrences(starts_at)
  WHERE status = 'pending' AND message_text IS NULL;

-- reconciler and day view: everything for a date
CREATE INDEX idx_occ_status_starts ON occurrences(status, starts_at);

-- per-item history, and the calendar range query when it names an item
CREATE INDEX idx_occ_item_starts ON occurrences(item_id, starts_at);

-- the calendar's unfiltered range query (migration 0006). Neither index above
-- leads with starts_at, so a bare date range used to walk one of them end to
-- end; measured with EXPLAIN QUERY PLAN, this one is the range seek.
CREATE INDEX idx_occ_starts ON occurrences(starts_at);

-- chain walking
CREATE INDEX idx_occ_parent ON occurrences(parent_occurrence_id)
  WHERE parent_occurrence_id IS NOT NULL;

-- active items, the set injected into every agent turn
CREATE INDEX idx_items_active ON items(active) WHERE archived_at IS NULL;

CREATE INDEX idx_conv_created ON conversations(created_at DESC);
CREATE INDEX idx_llm_created ON llm_calls(created_at DESC);

-- webhook update dedup (session 8, P1): a transport adapter that retries a
-- delivery it did not get a 2xx for must not produce two rows for the same
-- update. store.CreateConversation already guards this in application code,
-- inside the one writer transaction; this index is the backstop, on the same
-- "guard belongs in SQL, not caller discipline" reasoning idx_occ_due and
-- DeleteFuturePendingOccurrence's WHERE clause already use. Partial: a row
-- this service generated itself (an assistant reply, a tool result) has no
-- transport/external_id and is never a dedup candidate.
CREATE UNIQUE INDEX idx_conv_dedup ON conversations(transport, external_id)
  WHERE transport IS NOT NULL AND external_id IS NOT NULL;

-- P3.5: active goals, the set the briefing and context injection both read
CREATE INDEX idx_goals_active ON goals(status) WHERE status = 'active';
CREATE INDEX idx_goals_item ON goals(item_id) WHERE item_id IS NOT NULL;
CREATE INDEX idx_goal_updates_goal ON goal_updates(goal_id, created_at DESC);
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
| `goals` | Never deleted once terminal (`met`/`missed`/`abandoned`), same reasoning as `items.archived_at` |
| `goal_updates` | Retained with the goal, since it is the only record of trend for a freestanding goal |

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
