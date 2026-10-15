# Agent Specification

Three distinct model-driven components, deliberately separated because they have
different reliability requirements, different cost profiles, and different failure
modes.

| Component | Runs | Failure mode | Latency budget |
|---|---|---|---|
| **Conversational agent** | On inbound message | Tell the user, write nothing | Seconds, user is waiting |
| **Copywriter** | Ahead of each occurrence | Fall back to plain title | Minutes, nobody is waiting |
| **Reconciler composer** | Once daily | Fall back to a templated list | Seconds |
| **Briefing composer** (P3.5, session 19) | Once daily, ~30 min ahead of `BRIEFING_AT` | Fall back to a plain template built from context injection | Minutes, nobody is waiting |

## Conversational agent

### Tool catalog

Argument structs, from which the JSON Schema sent to the model is generated. Full
schema tags are shown on `CreateItemArgs` and elided elsewhere for readability;
every enum field carries them.

```go
type ListItemsArgs struct {
    Filter string `json:"filter,omitempty"` // active | all | paused, default active
}

type CreateItemArgs struct {
    Title              string   `json:"title" jsonschema:"required"`
    Schedule           Schedule `json:"schedule" jsonschema:"required"` // tagged union, 05-schedule-spec.md
    Notes              *string  `json:"notes,omitempty"`
    Kind               string   `json:"kind,omitempty"      jsonschema:"enum=reminder,enum=event,default=reminder"`
    TZ                 *string  `json:"tz,omitempty"`       // defaults to current device tz
    TZMode             string   `json:"tz_mode,omitempty"   jsonschema:"enum=fixed,enum=floating,default=floating"`
    NotifyPolicy       string   `json:"notify_policy,omitempty" jsonschema:"enum=at_time,enum=silent,enum=digest,default=at_time"`
    Priority           int      `json:"priority,omitempty"  jsonschema:"minimum=1,maximum=5,default=3"`
    GracePeriodMinutes *int     `json:"grace_period_minutes,omitempty"`
    ReconcileAt        *string  `json:"reconcile_at,omitempty"` // "HH:MM" local
}

type UpdateItemArgs struct {
    ItemID       string      `json:"item_id" jsonschema:"required"`
    Scope        string      `json:"scope,omitempty"` // future_all | from_date | single
    FromDate     *string     `json:"from_date,omitempty"`     // required when scope=from_date
    OccurrenceID *string     `json:"occurrence_id,omitempty"` // required when scope=single
    Changes      ItemChanges `json:"changes" jsonschema:"required"` // only fields being changed
}

type DeleteItemArgs struct {
    ItemID    string `json:"item_id" jsonschema:"required"`
    Confirmed bool   `json:"confirmed"` // must be true to execute
}

type BulkResolveArgs struct {
    Resolutions []Resolution `json:"resolutions" jsonschema:"required,minItems=1"`
}

type Resolution struct {
    OccurrenceID string  `json:"occurrence_id" jsonschema:"required"`
    Status       string  `json:"status" jsonschema:"required"` // completed | skipped | missed
    Note         *string `json:"note,omitempty"`
}

type SnoozeArgs struct {
    OccurrenceID string `json:"occurrence_id" jsonschema:"required"`
    Delta        string `json:"delta" jsonschema:"required"` // 10m | 1h | tonight | tomorrow
}

type PauseArgs struct {
    Scope  string  `json:"scope" jsonschema:"required"` // global | item
    Until  string  `json:"until" jsonschema:"required"` // ISO date
    ItemID *string `json:"item_id,omitempty"`
}

type SetTimezoneArgs struct {
    TZ string `json:"tz" jsonschema:"required"` // IANA
}

type GetStatsArgs struct {
    Range  string  `json:"range,omitempty"` // week | month | quarter | all, default month
    ItemID *string `json:"item_id,omitempty"`
}

type ProposeChangeArgs struct {
    ItemID    string      `json:"item_id" jsonschema:"required"`
    Proposal  ItemChanges `json:"proposal" jsonschema:"required"`
    Rationale string      `json:"rationale" jsonschema:"required"`
}

type RequestEscalationArgs struct {
    Reason string `json:"reason" jsonschema:"required"`
}

// P3.5, session 18 — see 11-goals-spec.md
type CreateGoalArgs struct {
    Title       string `json:"title" jsonschema:"required"`
    PeriodKind  string `json:"period_kind" jsonschema:"required,enum=day,enum=week,enum=month,enum=custom"`
    PeriodStart *string `json:"period_start,omitempty"` // defaults to today
    PeriodEnd   *string `json:"period_end,omitempty"`   // required when period_kind=custom
    ItemID      *string `json:"item_id,omitempty"`      // set for an item-linked goal
    TargetCount *int    `json:"target_count,omitempty"` // required when item_id is set
}

type UpdateGoalArgs struct {
    GoalID  string      `json:"goal_id" jsonschema:"required"`
    Changes GoalChanges `json:"changes" jsonschema:"required"`
}

type ListGoalsArgs struct {
    Filter string `json:"filter,omitempty"` // active | all, default active
}

type LogGoalProgressArgs struct {
    GoalID      string  `json:"goal_id" jsonschema:"required"`
    ProgressPct *int    `json:"progress_pct,omitempty" jsonschema:"minimum=0,maximum=100"`
    Note        *string `json:"note,omitempty"`
}
```

Returns: `CreateItemArgs` and `UpdateItemArgs` yield a result carrying the next
three concrete occurrences. `BulkResolveArgs` is atomic, all or nothing.
`GetStatsArgs` reads the same `chains` view as the dashboard. `ProposeChangeArgs`
and `RequestEscalationArgs` return nothing — the first surfaces a suggestion and
writes no rows, the second terminates the turn for retry at the next tier.

Optional fields are pointers rather than zero values throughout. This is the one
place Go is genuinely worse than the Python this spec was first written in: `0`
and `omitted` are the same value for a plain `int`, and `priority` has a
non-zero default, so a model that omits the field would otherwise silently get
priority zero and fail validation. Pointers make "absent" representable, and the
defaults get applied in one place after decoding rather than at each use.

Two of these deserve comment.

**`bulk_resolve` rather than repeated single calls.** Six sequential
`complete_item` calls means six chances to fail, six validation passes, and a
partial application when call four is wrong. One tool taking a list gives one
transaction and an all-or-nothing outcome, which is what "did everything except
the walk" actually needs.

It is built as of session 15, ahead of the rest of P3, and this argument is why.
The last P2 exit criterion — "did my stretching already" at 07:00 cancelling the
18:00 notification — needs the agent to resolve exactly one occurrence, and the
cheap answer would have been a single-occurrence tool. That is the tool this
paragraph rejects, so the smaller move was to bring `bulk_resolve` forward and
let a batch of one be the degenerate case it already is. Its endpoint,
`POST /api/occurrences/bulk-resolve`, stays in P3, because nothing calls it until
the web app.

In the implementation, `resolutions` is validated element by element at Layer 1 —
a rejection names `resolutions[2].status`, not `status` — and two rows naming the
same occurrence are refused outright rather than left to resolve it and then meet
themselves coming back.

**`propose_change` writes nothing.** The agent is allowed opinions about your
schedule and not allowed to act on them. An assistant that silently moves a
reminder because it inferred you would prefer 08:00 is a trust-destroying bug that
presents itself as a feature.

**`log_goal_progress` is a conversational turn, not a resolution (P3.5).** It
requires at least one of `progress_pct` or `note` and writes an append-only
`goal_updates` row rather than going through `bulk_resolve` or the status
state machine — a goal is not an occurrence, and "put the report at about
60%" has no terminal state to reach. See [11-goals-spec.md](11-goals-spec.md#progress-tracking).

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

Active goals:                                    [P3.5, session 18]
  gol_01H..  "gym 4x this week"   item-linked  2/4 this period
  gol_01H..  "ship the report"    freestanding 60%, updated 2026-08-04

Last touched: itm_01H.. ("evening walk")
Context ref:  reconcile:2026-08-05   [present only while a check-in awaits an answer]
  awaiting:  occ_01H..  morning stretch
             occ_01H..  evening walk
Context ref:  briefing:2026-08-05    [present while this morning's briefing awaits a reply, P3.5]
```

Today's occurrences with status are the addition that makes "everything except the
walk" work. Active item definitions alone do not tell the model what is
outstanding today, so "everything" would be unresolvable.

`Last touched` is what makes "make it more like five times" resolve without
naming the item again.

`Context ref` is the whole of reply recognition, and it is worth being explicit
that there is nothing else. No classifier decides whether a message answers the
check-in, and no separate route carries it: a reply is an ordinary inbound turn
through the ordinary catalog. Two things make it resolvable. The check-in is
already in cross-turn history — the reconciler writes it in the same shape
`persistAssistantProse` does — so the model sees its own question immediately
above the answer. And this block names what is still outstanding.

The block is present only while at least one occurrence the check-in asked about
is unresolved *and* inside its grace window, which is one read
(`store.ListAwaitingReconciliation`) shared with the pass that assigns `missed`.
Absent rather than empty when nothing is awaiting, the same convention `Last
touched` follows: an empty `Context ref` would tell the model it is answering a
question nobody asked.

Titles sit beside the ids and are not decoration. A 21:00 check-in answered at
00:15 is waiting on rows from *yesterday*, which `Today's occurrences` no longer
lists, and the behavioural rules forbid guessing an id — so past midnight this
block is the only surviving map from a name the user might say to an id the tool
needs.

What stops a new request being mistaken for an answer is that nothing is forced.
The rules state that a check-in does not oblige the user to answer it before
saying anything else, so "remind me to call mum at 6" arriving after one reaches
`create_item` exactly as it would have.

Active goals is read from the same `internal/conversation.Store` interface as
active items — a widened method, not a second read path — and is the block
both the agent and the (P3.5) briefing composer read, so a goal mentioned in
the morning briefing and a goal the agent discusses mid-conversation are
looking at identical numbers.

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

**Layer 1, schema.** `encoding/json` decode into the argument struct with
`DisallowUnknownFields`, then `go-playground/validator` for enums, ranges, and
required fields. The JSON Schema advertised to the model is generated from the
same structs by `invopop/jsonschema`, so what the model is told and what the
validator enforces cannot drift.

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
| `briefing` (P3.5) | Gemma 4 31B | Gemini 3.1 Flash Lite | off | Same shape as `reconcile`: composing one message from a list, not reasoning over it |
| `proposal` (P5) | Gemma 4 31B | Gemini 3.1 Flash Lite | off | Wording one proposal whose content code has already chosen; refused unless it states the alternative |

Thinking mode off for copywriting is deliberate. Reasoning tokens help with
"weekdays but not the week of the 14th" and do nothing for a one-line nudge except
cost.

The copywriter has no tier 2 because escalating a task whose failure mode is
already acceptable spends money to avoid an outcome that is fine.

### Interface

```go
func (c *Client) Complete(
    ctx context.Context,
    task Task,
    messages []Message,
    tools []Tool,
) (Result, error)
```

Configuration maps each `Task` to an ordered tier list. All providers behind one
OpenAI-compatible client with per-tier `base_url` and key, built on `net/http` —
no SDK, per D-021. OpenRouter is the default path, because one key and automatic
provider failover matter when running on free-tier hosting.

`ctx` carries the per-tier timeout. It matters most for the copywriter, whose
whole failure story is "give up and let the scheduler send the plain title" — a
hung request there would otherwise hold a slot until the occurrence fires anyway.

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

### Built in session 25 (`internal/copywriter`)

Eight notes from the implementation, each a place the spec left a choice open.

- **Nothing here is in the firing path.** The scheduler is unchanged: it reads
  `message_text` or the title. Every copywriter write is
  `store.WriteGeneratedMessage`, whose statement carries `AND status =
  'pending'` and `AND generation_pass = <previous pass>`, so a generation that
  finishes after the scheduler's claim is a zero-row update. It is counted as
  `late` and the text is discarded; nothing is rewritten.
- **G9 is a gate before any cost.** An item with fewer than ten resolved chains
  (Q-7) gets no model call, no counted attempt and no `llm_calls` row, and fires
  as its title. It is re-evaluated every tick, so there is no flag to flip when an
  item graduates.
- **Windows are "has started", not slots.** Pass 1 takes any text-less pending
  `at_time` row starting within 30 minutes (and more than 90 seconds out); pass 2
  takes a row with safety-net text starting within 4 minutes. A **snooze child**
  is therefore generated on the next tick after the snooze: its own T-30 passed
  before it existed, but it is already inside the window, and it gets the
  snooze-child strategy because it is one. A row with under 90 seconds left is
  not worth a call and fires as its title.
- **Pass 2 compares a fingerprint, not a timestamp.** `occurrences.message_basis`
  (migration 0008) stores a hash of what the text was written from: the tone
  strategy, title and notes, snooze-child flag, streak numbers, last seven
  outcomes, miss run, snoozes today, skip notes and scheduled time. Pass 2
  rebuilds the context and calls the model only when the hash differs. Not in it:
  `recent_messages`, which a rewrite itself changes, and the clock-derived
  `days_since_last_completion`. "Pause moved" needs no term: a pause deletes the
  pending rows in its window, a lifted one re-materializes them with no text, and
  the candidate read drops a paused item. "Is a snooze child" is in the
  fingerprint but cannot differ between passes, since pass 1 already knew. The
  spec's "a sibling *item* was missed" is read as the item's own other
  occurrences, which is what the context blob can see.
- **Attempts are per pass.** `generation_attempts` is incremented *before* the
  call (so a crash mid-call still costs one) and reset to zero by a successful
  write, so pass 2 has its own two. `llm_calls.occurrence_id` is the record of how
  many it really took. A pass-1 row that exhausted its attempts stays text-less
  and fires as its title; pass 2 only refreshes, it does not rescue.
- **The numbers are `stats`'.** `stats.Service.ItemSignals` computes the blob's
  figures with the same `streaks`, `Classify` and `medianLag` the stats page uses,
  so "12 in a row" in a reminder and on the page are one computation. The
  consecutive-miss run, which the tone ladder needs and the page does not, walks
  past skipped, awaiting and open chains exactly as the streak does.
- **The tone ladder is `copywriter.Choose`, in code**, in precedence order:
  snooze child, dormancy, shrink the ask, protect the streak, light
  acknowledgement, neutral. Dormancy is five consecutive misses, or fourteen days
  without a completion *and* at least two misses *and* no recent skip note - the
  spec's "14 days dormant" would otherwise call a monthly reminder dormant half
  of every month, or a person on a stated break gone quiet. The miss count is not
  sent to the model; the strategy is. G8 is a hard rule in the system prompt
  after `persona.md`, not only a line in it, and a message over 120 characters,
  empty, or identical to a recent one is refused rather than repaired.
- **The dormancy question moved (session 26).** Session 25 sent it as an
  ordinary reminder with no recorded context. It is now the proactive loop's, sent
  once per episode as a proposal with a `context_ref`, and the copywriter's
  dormancy branch (`Choose` is unchanged, and so are its thresholds, which are now
  `internal/stats`') writes the plainest reminder it can and asks nothing. One
  owner, so a dormant item is not asked twice, once with a record and once
  without.

`persona.md` is read by `defaults.GetPersona` on every call - from the agent's
system prompt, the check-in composer, the briefing composer and now the
copywriter. There is one function and no cache, so an edit reaches all four on
their next call.

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

Built in session 17 (`internal/reconciler/compose.go`). Four notes from the
implementation:

- **The model is given titles and nothing else** — no ids, no statuses, no
  times. The set of rows that gets a `reconciled_at` is built in `Reconcile`
  from the same slice, so whatever the model writes it cannot widen or narrow
  what was actually asked about. A composed message naming an item nobody
  gathered would be a question the grace window then never answers for.
- **The fallback is the value the code starts from**, not an error path it falls
  into: `composeCheckIn` computes the template first and replaces it only on a
  usable completion. There is no arrangement of failures that reaches the send
  with nothing to send. It is reached on a provider failure, on an empty
  completion, on one over the length guard, and on a deployment with no chat
  transport — which is the commonest case and not a degraded one, since a
  check-in nobody can reply to has no use for better prose.
- **An empty or over-long completion falls back rather than being repaired.**
  `model.Complete` reports prose-with-no-content as a *success*, because
  adequacy is the caller's judgement; this component is where that judgement
  gets made. Truncating an over-long answer would ship the first 400 runes of
  something that misunderstood the task.
- **The whole tier walk is budgeted at 20 seconds**, far under the tiers' own
  configured timeouts. Supervisor ticks are serial, so an unbudgeted two-tier
  walk would hold the reconciler loop for minutes — delaying the check-in,
  stalling the grace pass behind it, and staling `/healthz`. The component with
  the good failure mode is the one that should give up first.

## Briefing composer (P3.5, session 19)

Runs once daily, roughly 30 minutes ahead of the configured `BRIEFING_AT`,
reusing the copywriter's shape rather than inventing a third generation
pattern: a safety-net pass with time to spare, no refresh pass, and a plain
fallback on failure. Composed from the same context blob context injection
assembles for the agent — active items, today's occurrences, active goals and
their current progress — not a second read of the same data.

```
Wednesday. Vitamins and the evening walk are on deck; morning stretch is
silent so it won't ping. Gym goal is at 2 of 4 for the week. Anything else
you want today to include?
```

Rules:

- States what today looks like, not a generic greeting — the point is
  information, and "what should today include" is a real question expecting
  a real answer, not a rhetorical one.
- Names anything that broke from the normal routine explicitly: an item
  resuming from a pause, a schedule changed since yesterday, a goal at risk
  of missing its period. This is the one place the briefing goes beyond what
  the reconciler or copywriter already state, because it is the one surface
  looking a full day ahead rather than at a single item or a single day's
  leftovers.
- Sent with `context_ref = briefing:{date}`, mirroring reconciliation's
  `context_ref = reconcile:{date}`, so a reply is recognised as answering the
  briefing rather than starting a new request. Recognition is a context-block
  line (`renderBriefingContextRef`) and `store.HasInboundSince` in the grace
  pass — no classifier, no separate route.
- Falls back to a plain templated summary if the model call fails, if there is
  no model client at all, or if the compose window was missed — the same item
  and goal lists, rendered without a model, never silence.
- Tone on non-response was [Q-16](10-open-questions.md#q-16-tone-for-an-unanswered-morning-briefing),
  resolved in session 19 as *G8 wins outright*: on repeated non-response the
  briefing may be shorter and more direct, never guilt or scolding. Nothing
  about composition or fallback depended on the answer; the loop only records
  *whether* a reply arrived.

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
| Morning briefing unanswered past grace (P3.5) | Next briefing shorter and more direct — never guilt or scolding ([Q-16](10-open-questions.md#q-16-tone-for-an-unanswered-morning-briefing), resolved session 19) |
| Goal at risk of missing its period, P3.5 | Named in the next morning briefing, not a separate interruption |

Cap unprompted messages at three per day, tracked in `kv.proactive_count:{date}`.
Personality is the feature most likely to charm for a fortnight and then get
muted, and the failure mode is volume rather than tone.

### Built in session 26 (`internal/proactive`)

The rows above that say `propose_change` are one loop, the seventh, and these are
the decisions it makes.

- **Which loop, and when.** `internal/proactive`, every five minutes. The sweeper
  was the obvious home and is hourly, has no transport, and is the loop that
  cannot reach the model; this one has the briefing's shape. It sends only between
  09:00 and 20:00 in the person's zone (`proactive.WindowStart` / `WindowEnd`,
  start inclusive), which sits between the default briefing and check-in. A trigger
  detected at 03:00 is latched and sent at 09:00. Nothing is sent during a global
  pause.
- **Definitions are `internal/stats`'.** Five consecutive misses is
  `ItemSignals.ConsecutiveMisses`, the run the streak rules define: a skip, an
  awaiting chain and an open chain are neutral, a completion ends it. Dormant is
  `stats.IsDormant`: no completion in 14 days, at least 2 misses, no recent skip
  note. "No resolution of any kind" was rejected as the definition because a sparse
  schedule produces it. The two are mutually exclusive in the loop (five misses is
  the first), and share one latch per item, so an item is asked once.
- **Three snoozes against the cap.** The cap is checked when a snooze is
  requested: a link at depth `snooze_cap` that is snoozed again resolves `missed`.
  So at the default cap of 3, "three snoozes on one occurrence" is the chain
  sitting at depth 3 - the last moment a proposal can save it from being assigned a
  miss - and not the exhaustion itself. The threshold is `min(3, snooze_cap)`, and
  an item with cap 0 never triggers. It is evaluated per chain over the last 48
  hours and fires whether the chain later completes or misses, because it is about
  the time.
- **The alternative is code's.** A retime for a snooze (a fixed schedule's `at`,
  or a windowed/fuzzy window start, to the time the chain was pushed to, rounded to
  a quarter hour); a smaller ask for the others (half a fuzzy count, every other
  day of a day list, Monday-Wednesday-Friday for a daily rule); a drop where
  nothing can be made smaller. A proposed schedule is checked with
  `schedule.Validate`, so it is one the ordinary `update_item` would accept.
- **A proposal persists as `kv.proposals` plus a `conversations` row with
  `context_ref = proposal:{id}`.** The record holds the exact tool call a "yes"
  would be. The agent's context block lists every open proposal, its item and that
  call; `behaviouralRules` says to make it, once, only on a plain agreement. It is
  on offer for one reply that runs a tool (`PassProposals` at the end of the turn)
  and for 24 hours at most, and an apology or rephrase does not spend it. Nothing
  in the system applies a proposal: the only route is an ordinary `update_item` or
  `delete_item` on a user's message. The limit of that is the model's reading of
  "yes"; the code bounds what a misreading can do to one turn, one item, and the
  listed arguments.
- **Composition is template-first.** The template states the alternative and
  offers leaving things as they are. A model completion replaces it only if it
  names the item and contains every `Mentions` group (`proactive.Accept`), so a
  message cannot offer something other than what a "yes" does.
- **Latches.** One per episode, `detected` then `asked`. Reset by the condition
  clearing (a completion ends a miss run), by the item going away, and by time: an
  asked episode still true after 30 days is a new one. A reply does not reset it,
  and a pause neither creates nor clears one.
- **The cap.** Three per local day, and only the proactive loop's sends count. The
  check-in and the briefing are scheduled by the person and a reply to something
  they said is not unprompted; if the first two counted, three would leave one. An
  agent-initiated `propose_change` is the second kind and is uncapped. Over the
  cap a trigger is held, not dropped: it stays `detected` and goes out on the first
  in-window pass of a day with budget, counted once per day as capped. One message
  per pass, five minutes apart.
- **A known gap.** The prompt says to call `list_items` when a proposal is
  declined or acknowledged and there is nothing to do, because every reply must be
  a tool call and there is no no-op tool. "No thanks" therefore answers with the
  item list. A harmless acknowledgement tool is the follow-up.
