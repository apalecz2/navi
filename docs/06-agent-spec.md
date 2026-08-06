# Agent Specification

Three distinct model-driven components, deliberately separated because they have
different reliability requirements, different cost profiles, and different failure
modes.

| Component | Runs | Failure mode | Latency budget |
|---|---|---|---|
| **Conversational agent** | On inbound message | Tell the user, write nothing | Seconds, user is waiting |
| **Copywriter** | Ahead of each occurrence | Fall back to plain title | Minutes, nobody is waiting |
| **Reconciler composer** | Once daily | Fall back to a templated list | Seconds |

## Conversational agent

### Tool catalog

```python
list_items(
    filter: Literal["active", "all", "paused"] = "active",
) -> list[ItemSummary]

create_item(
    title: str,
    schedule: Schedule,              # tagged union, see 05-schedule-spec.md
    notes: str | None = None,
    kind: Literal["reminder", "event"] = "reminder",
    tz: str | None = None,           # defaults to current device tz
    tz_mode: Literal["fixed", "floating"] = "floating",
    notify_policy: Literal["at_time", "silent", "digest"] = "at_time",
    priority: int = 3,
    grace_period_minutes: int | None = None,
    reconcile_at: str | None = None,
) -> CreateResult                    # includes next 3 concrete occurrences

update_item(
    item_id: str,
    scope: Literal["future_all", "from_date", "single"] = "future_all",
    from_date: str | None = None,    # required when scope="from_date"
    occurrence_id: str | None = None,# required when scope="single"
    changes: ItemChanges,            # only fields being changed
) -> UpdateResult                    # includes next 3 concrete occurrences

delete_item(
    item_id: str,
    confirmed: bool = False,         # must be True to execute
) -> DeleteResult

bulk_resolve(
    resolutions: list[Resolution],   # {occurrence_id, status, note}
) -> BulkResolveResult               # atomic; all or nothing

snooze(
    occurrence_id: str,
    delta: Literal["10m", "1h", "tonight", "tomorrow"],
) -> SnoozeResult

pause(
    scope: Literal["global", "item"],
    until: str,                      # ISO date
    item_id: str | None = None,
) -> PauseResult

set_timezone(
    tz: str,                         # IANA
) -> TimezoneResult                  # reports how many floating items shifted

get_stats(
    range: Literal["week", "month", "quarter", "all"] = "month",
    item_id: str | None = None,
) -> Stats                           # reads the same `chains` view as the dashboard

propose_change(
    item_id: str,
    proposal: ItemChanges,
    rationale: str,
) -> None                            # writes nothing; surfaces a suggestion

request_escalation(
    reason: str,
) -> None                            # terminates the turn, retries at the next tier
```

Two of these deserve comment.

**`bulk_resolve` rather than repeated single calls.** Six sequential
`complete_item` calls means six chances to fail, six validation passes, and a
partial application when call four is wrong. One tool taking a list gives one
transaction and an all-or-nothing outcome, which is what "did everything except
the walk" actually needs.

**`propose_change` writes nothing.** The agent is allowed opinions about your
schedule and not allowed to act on them. An assistant that silently moves a
reminder because it inferred you would prefer 08:00 is a trust-destroying bug that
presents itself as a feature.

### Context injection

Every turn carries:

```
Current time:      2026-08-05T14:32:00-04:00 (Wednesday)
Device timezone:   America/Toronto
Global pause:      none

Active items:
  itm_01H..  "morning stretch"   silent   fuzzy 7/week, window 06:00-11:00
  itm_01H..  "vitamins"          at_time  fixed daily 09:00
  itm_01H..  "evening walk"      at_time  windowed Mo-Fr, 17:00-21:00
  ...

Today's occurrences:
  occ_01H..  09:00  vitamins         completed  (web, 09:12)
  occ_01H..  07:30  morning stretch  pending
  occ_01H..  18:40  evening walk     pending

Last touched: itm_01H.. ("evening walk")
Context ref:  reconcile:2026-08-05   [present only when replying to a check-in]
```

Today's occurrences with status are the addition that makes "everything except the
walk" work. Active item definitions alone do not tell the model what is
outstanding today, so "everything" would be unresolvable.

`Last touched` is what makes "make it more like five times" resolve without
naming the item again.

### System prompt structure

Assembled from parts rather than being one string, so each can be tuned
independently:

1. **Role and scope.** What the system is, what it manages, what it does not do.
2. **Persona.** From `get_persona()`, shared with the copywriter so the voice is
   consistent across chat and notifications.
3. **Vocabulary defaults.** Rendered from `get_defaults()`, so the model reads the
   same resolution table the validator enforces.

Both are function calls rather than inline file reads. `get_persona()` reads
`/config/persona.md` and `get_defaults()` reads `/config/defaults.yaml`, and that
is the whole implementation today. The indirection earns its keep immediately —
hot-editing the persona without a rebuild is a P5 exit criterion, so the read
needs a single home with the caching decision in it either way — and it is also
the seam where those values would stop being files if they ever needed to differ
per person. Callers should not know which it is.
4. **Behavioural rules.** The list below.
5. **Injected context.** As above.

### Behavioural rules

- **Never ask a clarifying question about an under-specified schedule.** Apply the
  defaults, state the interpretation, invite correction. Ask only when a
  *reference* is ambiguous, such as two items that could both be "the gym one".
- **Always confirm a write in plain language, with the next three concrete
  timestamps.** "Three times a week at random times" is hard to check. "Tue 2:15pm,
  Thu 10:40am, Sat 4:05pm" is checkable at a glance. This is the single cheapest
  guard against a silent misparse.
- **State every inferred parameter.** If the window, count, or gap was guessed,
  say so in the confirmation.
- **Confirm before deleting.** Never on the first turn.
- **Prefer `bulk_resolve` for any message containing more than one completion.**
- **Prefer `pause` over multiple skips** when the user indicates absence.
- **Use `propose_change` to suggest, never `update_item` to impose.**
- **Call `request_escalation`** when the request is ambiguous, spans multiple
  items in a way that is hard to disentangle, or references something unresolvable.

### Validation

Applied to every tool call before any write. Code, not judgement.

**Layer 1, schema.** Pydantic model for the arguments. Types, enums, required
fields.

**Layer 2, semantic.** The table in
[05-schedule-spec.md](05-schedule-spec.md#validation): RRULE parses and produces
occurrences, times are in the future and bounded, windows are ordered and wide
enough, gaps are satisfiable, referenced ids exist.

**Layer 3, transactional.** The whole write, including occurrence
re-materialization, happens in one transaction. A failure at any point leaves
nothing behind.

### Escalation ladder

```
attempt at tier 1
  │
  ├─ validation passes ─────────────────────────▶ execute, confirm
  │
  └─ validation fails
       │
       └─ retry at tier 1, validation error appended to messages
            │
            ├─ passes ────────────────────────────▶ execute, confirm
            │
            └─ fails
                 │
                 └─ escalate to tier 2, both errors appended
                      │
                      ├─ passes ───────────────────▶ execute, confirm
                      │
                      └─ fails ────────────────────▶ ask user to rephrase,
                                                     write nothing
```

The same-tier retry is worth its cost: models usually fix their own schema errors
when shown the error text, and it is far cheaper than a tier-two call.

**Escalation triggers beyond validation failure:**

| Trigger | Rationale |
|---|---|
| `request_escalation` called | Models are reasonably calibrated about their own uncertainty; a self-report is free compared to a failed attempt |
| No tool call when a write was expected | Prose in response to "remind me to X" is a failure, not an answer |
| More than three distinct intents in one turn | Cheap pre-classifier; complex multi-intent turns start at tier 2 |

### Model routing

| Task | Tier 1 | Tier 2 | Thinking | Notes |
|---|---|---|---|---|
| `crud` | Gemma 4 31B | Gemini 3.1 Flash Lite | on | Date reasoning and RRULE construction benefit from it |
| `bulk_resolve` | Gemma 4 31B | Gemini 3.1 Flash Lite | off | Matching names to a provided list is structurally simple |
| `copywriter` | Gemma 4 31B | none | off | Failure is invisible thanks to the plain-title fallback, so no tier 2 |
| `reconcile` | Gemma 4 31B | Gemini 3.1 Flash Lite | off | Composing one short question from a list |
| `digest` | Gemini 3.1 Flash Lite | larger | on | Weekly, actually reasons over statistics, cost is negligible at that frequency |

Thinking mode off for copywriting is deliberate. Reasoning tokens help with
"weekdays but not the week of the 14th" and do nothing for a one-line nudge except
cost.

The copywriter has no tier 2 because escalating a task whose failure mode is
already acceptable spends money to avoid an outcome that is fine.

### Interface

```python
async def complete(
    task: Task,
    messages: list[Message],
    tools: list[Tool] | None = None,
) -> Result
```

Configuration maps each `Task` to an ordered tier list. All providers behind one
OpenAI-compatible client with per-tier `base_url` and key. OpenRouter is the
default path, because one key and automatic provider failover matter when running
on free-tier hosting.

Every call is logged to `llm_calls`. After two weeks, tier-one success rate per
task becomes a query, and the ladder gets tuned from data rather than intuition.
This log is the reason the tiering is worth building at all.

## Copywriter

### Two passes

| Pass | Timing | Purpose |
|---|---|---|
| 1 | T minus 30 minutes | Safety net. Guarantees text exists. |
| 2 | T minus 4 minutes | Refresh, only if relevant state changed since pass 1 |

"Relevant state changed" means: the item was resolved early, a sibling item was
missed or skipped today, the occurrence is a snooze child, or the pause state
changed.

Loop interval is 60 seconds. `generation_attempts` caps at 2 per pass. On
exhaustion the field stays as it is, and if it is null the scheduler sends the
plain title.

Cost is roughly double a single-pass design, on the free-tier Gemma path, which is
to say nothing.

### Context blob

Assembled per occurrence from the `chains` view:

```json
{
  "title": "evening walk",
  "notes": null,
  "scheduled_local": "2026-08-05T18:40:00-04:00",
  "is_snooze_child": false,
  "current_streak": 0,
  "longest_streak": 11,
  "last_7": ["completed", "completed", "missed", "missed",
             "missed", "completed", "skipped"],
  "completion_rate_30d": 0.52,
  "days_since_last_completion": 3,
  "median_lag_minutes": 24,
  "snoozes_today": 0,
  "recent_skip_notes": ["on vacation"],
  "recent_messages": ["...", "...", "..."]
}
```

`recent_messages` is the last three to five generated messages for this item,
passed in with an instruction not to reuse their angle. Without it you get four
phrasings on rotation and stop reading them inside a fortnight, which is the real
failure mode for this feature.

`recent_skip_notes` is what stops the next morning's message from being
passive-aggressive about a walk that was deliberately skipped for a stated reason.

### Tone ladder

Branch the prompt on the context blob. The interesting version of this feature is
varied *strategy*, not varied phrasing.

| State | Strategy |
|---|---|
| `current_streak >= 5` | Name the number, frame it as something to protect |
| `current_streak >= 1` | Light acknowledgement, keep it short |
| Single recent miss | Neutral. Do not mention the miss. |
| 2 to 4 consecutive misses | Shrink the ask. "Just five minutes today." |
| 5+ misses, or 14 days dormant | Stop nudging. Ask whether to reschedule, shrink, or drop. Route the reply back through the agent as a normal turn. |
| Snooze child | Acknowledge it is a second attempt, keep it lighter than the first |
| Recent skip with a note | Do not reference the skip at all |

The dormancy branch is what makes this better than a normal reminder app. A
reminder ignored for three weeks is a bug in the schedule, not a failure of
character, and the system is the only thing positioned to notice.

### `persona.md`

Mounted read-only, editable without a rebuild, because voice gets iterated on
twenty times.

Contents:

1. Voice description in two or three sentences
2. Hard length limit (under 120 characters)
3. Banned phrases. This is where "You've got this", "Let's crush it", and
   "Time to shine" get killed.
4. Six to ten worked examples covering different ladder states. Examples do
   substantially more work than adjectives.
5. Hard rule: never guilt, never scold.

The last one is not decoration. A model told to "be tough on me" overshoots, and
punitive copy at 07:00 is unpleasant in a way that gets the whole feature muted.

## Reconciler composer

Runs at the configured local time. Gathers everything unresolved for the day,
covering both silent items and `at_time` items that were notified and ignored, and
composes one message.

```
Haven't heard about stretching, vitamins, or the evening walk.
Which of those got done?
```

Rules:

- One message, never one per item. Consolidation is what makes the app stop
  feeling naggy and is what earns it the right to have a personality at all.
- Never accusatory. It is a question, not an audit.
- Sent with `context_ref = reconcile:{date}`, so the reply is recognised as an
  answer rather than a new request.
- Falls back to a plain templated list if the model call fails.
- The reply is handled by `bulk_resolve`, so reconciliation and batch completion
  are the same code path.

## Proactive behaviour

Triggers are deterministic conditions evaluated in code. Responses are generated.
This split is what keeps an agent with a personality from becoming an agent with
unpredictable behaviour.

| Trigger | Response |
|---|---|
| Reconciliation time, anything unresolved | Composed check-in |
| 5 consecutive misses on an item | `propose_change` with a specific alternative |
| 3 snoozes on one occurrence | `propose_change` suggesting a different time |
| 14 days dormant on an active item | Ask whether to keep it |
| Weekly, Sunday evening | Digest with statistics and one observation |

Cap unprompted messages at three per day, tracked in `kv.proactive_count:{date}`.
Personality is the feature most likely to charm for a fortnight and then get
muted, and the failure mode is volume rather than tone.
