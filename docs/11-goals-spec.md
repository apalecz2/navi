# Goals and Briefing Specification

This document defines what a goal is, how progress on it is tracked and
evaluated, and the morning briefing — the one proactive surface in this system
that expects a reply rather than just delivering one. Both are new relative to
[06-agent-spec.md](06-agent-spec.md)'s reconciler and copywriter, and both
build on mechanisms those already established rather than inventing parallel
ones.

**Status: built.** Goals landed in session 18, the morning briefing in session
19, closing [P3.5](09-roadmap.md#p35-goals--briefing). This document is now a
record of the shipped design rather than a forward specification; where the
implementation diverged from the text below it is noted inline. See D-024 and
D-025 for why goals are their own entity and why this waited for real completion
data.

## What a goal is

An item is the definition of a recurring *thing you do*. A goal is a target
*over a period* — it does not fire, it is not notified, and completing it once
does not resolve it. That difference is why goals are not a third
`items.kind`: the materializer, scheduler, and status state machine all assume
a thing that expands into occurrence rows with a single fire instant, and a
goal has neither. See [D-024](08-decisions.md#d-024-goals-are-a-first-class-entity-not-a-third-item-kind).

A goal is one of two shapes, both represented by the same table:

**Item-linked.** "Gym four times this week." The target is a count against an
existing item's `chains`, over the goal's period. No separate progress
mechanism — evaluation is a query against data that already exists, the same
`chains` view the dashboard and `get_stats` already read.

**Freestanding.** "Ship the report by Friday." No underlying item, because
not everything worth tracking is a recurring reminder. Progress is
self-reported through conversation, append-only, the way `conversations`
already logs every turn rather than mutating a single "current message"
field.

Both shapes carry a period (`day`, `week`, `month`, or a custom range) and a
status. Nothing about materialization, the scheduler, or notification policy
applies to either — a goal is read by the briefing and the agent, and written
by the agent; it is never the subject of the fire path.

## Schema

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

CREATE INDEX idx_goals_active ON goals(status) WHERE status = 'active';
CREATE INDEX idx_goals_item ON goals(item_id) WHERE item_id IS NOT NULL;
```

`target_count IS NULL XOR item_id IS NULL` is a semantic rule enforced at the
validation layer, not a `CHECK` — SQLite's `CHECK` can express the null pair
but the clearer error belongs in Go, next to every other rule in the
[validation table](#validation) below.

Progress on a freestanding goal is a separate, append-only table rather than
a mutable column on `goals`, for the same reason `conversations` is a log and
not a single "last message" field: the trail is the data. A `progress_pct`
column that gets overwritten on every check-in cannot answer "was this
trending up or down before it slipped."

```sql
CREATE TABLE goal_updates (
  id            TEXT PRIMARY KEY,
  goal_id       TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
  note          TEXT,                      -- freeform, e.g. "first draft done"
  progress_pct  INTEGER CHECK (progress_pct BETWEEN 0 AND 100),
  source        TEXT NOT NULL CHECK (source IN ('agent', 'web', 'briefing')),
  created_at    TEXT NOT NULL
);

CREATE INDEX idx_goal_updates_goal ON goal_updates(goal_id, created_at DESC);
```

A freestanding goal's current progress is the most recent `goal_updates` row
for it — a read, not a stored value, so it can never drift from the log that
produced it. An item-linked goal has no `goal_updates` rows at all; its
progress is always computed fresh from `chains`.

## Lifecycle

```
active ──▶ met         (period ends, target reached)
   │
   ├────▶ missed        (period ends, target not reached)
   │
   └────▶ abandoned      (explicit user instruction, any time)
```

No `pending` state, because a goal is active from creation — there is nothing
equivalent to "materialized but not yet due." No `snoozed`, because pushing a
goal's deadline is `update_item`-shaped (change `period_end`), not a
resolution.

**Evaluation is code, not the clock**, the same principle K6 already applies
to `missed` on occurrences: a goal transitions out of `active` only when the
sweeper's daily pass reads `period_end < today` and computes the outcome, not
because a date silently passed. Evaluation for an item-linked goal is
`count(chain.was_completed) >= target_count` over the goal's period; for a
freestanding goal it is whichever of `met` or `missed` the most recent
`goal_updates.progress_pct` implies (100 or a `note` the agent reads as
complete), falling to `missed` if the period ends with no update at all.

A goal that ends `missed` is not deleted — same reasoning as `archived_at` on
items: the statistics need it, and history is immutable.

## Progress tracking

**Item-linked**: none needed. `count * min_gap_hours`-style arithmetic against
`chains` already answers "how many of the four gym sessions happened this
week" without a write, the same way `get_stats` already answers completion
rate without a dedicated aggregation table.

**Freestanding**: a `log_goal_progress` tool call, added to the catalog
alongside `create_goal`, `update_goal`, and `list_goals`:

```go
type LogGoalProgressArgs struct {
    GoalID       string  `json:"goal_id" jsonschema:"required"`
    ProgressPct  *int    `json:"progress_pct,omitempty" jsonschema:"minimum=0,maximum=100"`
    Note         *string `json:"note,omitempty"`
}
```

At least one of `progress_pct` or `note` is required — a call with neither
writes nothing and returns a validation error, the same shape as any other
under-specified tool call. This is a normal conversational turn, not a
resolution: "put the report at about 60%" is `log_goal_progress`, not
`bulk_resolve`, because a goal is not an occurrence and does not go through
the status state machine at all.

## Velocity

Streak (`current_streak`, `longest_streak`) already answers "how consistent
have I been," computed per item over `chains`. It says nothing about *rate
toward a target* — four gym sessions in the last two weeks is a fine streak
and a missed goal if the target was ten.

**Velocity** is defined per item-linked goal as completions-per-period against
`target_count`, read the same way everything else in this document reads
progress: as a query over `chains`, filtered to the goal's `item_id` and
period, never as a stored, incrementally-updated counter. Storing a running
total invites exactly the drift class D-024's "chains, not rows" reasoning
already rejected once for streaks — a counter that can silently disagree with
the rows it was supposed to summarize.

For freestanding goals, velocity has no computable form — there is no
completion event to rate — and the closest equivalent is the slope of
`progress_pct` across `goal_updates`, which is a chart concern (below), not a
number the agent states as fact.

### Weekday-aware streaks: no new mechanism

A streak already only counts occurrences that were scheduled to exist. An
item whose `windowed` or `fixed` schedule only fires Monday through Friday
already produces no Saturday or Sunday occurrence to break the chain on — the
streak *is* a weekday streak by construction, because [chains](04-data-model.md#streaks-and-snooze-chains)
walks `occurrences`, and `occurrences` only has rows for days the schedule
said to fire on. Nothing here adds a "weekday mode" to streak computation;
the only open item is display — whether the UI states "12" or "12 weekdays"
— which is a P4 wording decision, not a schema or query change.

## Visualization

Goal progress and velocity are additional series on the statistics view
[04-data-model.md](04-data-model.md) and [09-roadmap.md](09-roadmap.md#p4-interfaces)
already scope for P4 — not a separate dashboard. `GET /api/stats/summary`
gains goal rows alongside item rows; `get_stats` reads the same query, so V6's
"agent's numbers match the dashboard's numbers" holds for goals without a
second code path. See [07-api-spec.md](07-api-spec.md#goals) for the endpoint
shapes.

## The morning briefing

A daily message, sent at a configured local time, that states what today
looks like — active items due today, goals in progress, anything that broke
from the normal routine (a paused item resuming, a schedule change made
yesterday) — and, unlike every other proactive message in this system,
expects a reply.

### Composition respects invariant 1

The briefing is composed **ahead of** the send time and stored, exactly like
occurrence `message_text` — never generated at fire time. A briefing loop runs
on the same shape as the copywriter's two-pass design: a safety-net pass with
time to spare, and nothing else, because unlike an occurrence there is no
mid-window state change worth a refresh pass for. On generation failure it
falls back to a plain templated summary — the item list and goal list Context
injection already assembles for the agent, rendered without a model call —
the same degrade-to-boring rule N3 gives reminders.

```
morning briefing loop (internal/briefing, daily, 60s tick, three phases)
  T minus ~30 minutes: compose from the same context blob the agent's
    system prompt uses (active items, today's occurrences, active goals
    and their current progress), store in kv.briefing_pending
  BRIEFING_AT: send kv.briefing_pending (or the template synchronously if
    it is absent, no model call); in one tx write the conversations row
    with context_ref = briefing:{date}, kv.last_briefing_date = {date},
    kv.briefing_awaiting_response = {date}\t{sent_at}, delete the pending slot
  every tick: clear kv.briefing_awaiting_response on any inbound message
    after sent_at; else record unanswered once the grace window closes
```

**As built (session 19):** the three `kv` slots are singletons carrying the
date in their value rather than `key:{date}` families — the `last_reconcile_date`
pattern this section cites. `kv.awaiting_response:{date}` in the prose below is
`kv.briefing_awaiting_response`. The response check runs on the briefing loop
itself, not the sweeper.

This is deliberately not a new occurrence kind. There is no `starts_at` row
to claim with `BEGIN IMMEDIATE` for something that happens once a day at one
global time with no per-item variation — a `kv` key recording whether today's
briefing has been sent and answered is the `last_reconcile_date` pattern
already in use, not a reason to bend the occurrence model to fit a single
daily event.

### Response tracking

`kv.briefing_awaiting_response` is set when the briefing sends and cleared the
moment any inbound message arrives after `sent_at` (`store.HasInboundSince`,
a `role='user'` row — the message is never parsed, because the briefing asks an
open question and engagement is the signal). The briefing is already in
cross-turn history and carries `context_ref = briefing:{date}`, and the agent's
context block gets a `Context ref: briefing:{date}` line while the marker is set
— recognition is context injection, the same as reconciliation, not a classifier
or a separate route. The briefing loop's own grace pass (same shape as K6's
window for `missed`) reads the marker once its grace period closes and evaluates
it as unanswered.

This mirrors reconciliation's grace-then-act shape deliberately: **triggers
are deterministic, responses are generative** (invariant 3) applies here
exactly as it applies to reconciliation's escalation to `missed`. What differs
is what "unanswered" produces — reconciliation writes `missed` on occurrences,
which is a state-machine transition. A briefing has no occurrence to
transition; going unanswered produces nothing but the confirmation that it
did, which is read by the day's tone strategy.

### Tone on non-response — resolved, session 19

[Q-16](10-open-questions.md#q-16-tone-for-an-unanswered-morning-briefing) was
resolved as **G8 wins outright**: on repeated non-response the briefing may
become shorter and more direct — lead with the decision that most needs one,
drop the softening, ask plainly — but never anything a reasonable reading calls
guilt or scolding. A minimal `config/persona.md` draft written this session
carries the rule; P5 expands the file and may revisit the wording, not the
principle. Nothing in the schema or the loop depended on the answer — the
`briefing_awaiting_response` mechanism only records *whether* a reply arrived.

## Validation

Applied before any write, same three-layer shape as schedules
([06-agent-spec.md](06-agent-spec.md#validation)):

| Check | Rule |
|---|---|
| Period ordered | `period_start <= period_end` |
| Period kind matches range | `week`/`month` periods align to calendar weeks/months unless `custom` |
| Item-linked goals have a target | `item_id IS NOT NULL` implies `target_count IS NOT NULL` and `>= 1` |
| Freestanding goals have no target | `item_id IS NULL` implies `target_count IS NULL` |
| Referenced item exists | `item_id`, when set, resolves to a non-archived item |
| `log_goal_progress` has content | At least one of `progress_pct`, `note` is present |
| Goal not already terminal | `update_goal` and `log_goal_progress` reject a goal in `met`, `missed`, or `abandoned` |

## What P3.5 inherits from earlier phases

- `internal/conversation.Store`'s named context-injection methods
  ([session 12](../CLAUDE.md)) are the interface a goals context read widens,
  not a second copy of "what's active" to keep in sync.
- The reconciler's grace-window shape (K6, D-008) is reused verbatim for
  response tracking rather than re-derived.
- `chains` (04-data-model.md) is reused verbatim for item-linked goal
  progress and velocity — no new aggregation table, per V6's "one code path"
  rule.
- The copywriter's two-pass, fallback-to-plain-text shape is reused for the
  briefing composer, satisfying invariant 1 the same way occurrence messages
  do.
