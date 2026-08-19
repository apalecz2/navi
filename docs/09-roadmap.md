# Roadmap

Phases are ordered by dependency and by risk, not by how interesting they are.
The boring core comes first, because a reliable scheduler with a clunky interface
is a working product and a great agent on a scheduler that drops reminders is not.

Each phase ends with a state worth living in. Nothing here requires the next phase
to be useful.

---

## P0: The boring core

**Goal.** A hardcoded reminder reliably reaches the phone.

- SQLite schema and migrations
- Item and occurrence models, status state machine
- Schedule spec parsing and validation for all four kinds
- Materializer, including the fuzzy placement algorithm and partial periods
- Timezone resolution, both modes, with DST edge cases handled
- Scheduler loop with `BEGIN IMMEDIATE` claiming and restart recovery
- Transport interface and capability flags, with the Telegram outbound half:
  `Send` only, plain bodies, no buttons yet
- Docker Compose, Cloudflare Tunnel ingress, Litestream to R2
- Repository module: all SQL behind it from the first query, sqlc wired up
- Loop supervisor: per-tick `recover`, context cancellation, `SIGTERM` drain (D12)
- First-run secret generation into `/data` (D10) — the helper, which has nothing to
  generate until the calendar token in P6. Built here because D10 is a rule about
  how secrets come into existence, and rules adopted after the first secret ships
  are rules that get an exception written for the first secret
- `/healthz`, `/metrics`, and a Grafana dashboard with delivery latency and loop
  ticks on it

**Exit criteria**

- [ ] A `fixed` daily reminder fires within a minute, every day, for three days —
      wall-clock bound, not checkable in one sitting. Not run this session; the
      service needs to be left running on the real host starting from the day
      this box gets ticked, three full days forward, before it can be marked
      done.
- [x] A `fuzzy` 3-per-week reminder produces three well-spread occurrences —
      verified via `cmd/naviseed`'s placement output (session 7): week 2026-W33
      placed Mon 13:25, Wed 19:50, Fri 15:25, each gap over the 20h minimum.
- [x] Restarting the container mid-day loses nothing and fires nothing twice —
      verified for real (session 7): an override occurrence was inserted,
      `docker compose restart navi` run mid-cycle before it was due, and it
      fired exactly once (`claimed:1, sent:1`) afterward. See
      `ops/restore-runbook.md`.
- [x] `litestream restore` into a clean directory reproduces the database —
      verified for real (session 7) against a local file-type replica (no R2
      bucket exists yet): restored copy's row counts, integrity check, and full
      `sqlite3 .dump` hash all matched the original exactly. Real R2
      connectivity itself is still unverified — see the runbook's "what this
      session did not verify."
- [x] The image is `scratch`, the binary is static, and timezones resolve inside
      it — re-verified (session 7) with Litestream now wrapping the entrypoint:
      `docker inspect` shows no base-image layers, the litestream binary is
      confirmed statically linked, and `DEFAULT_TZ=America/Toronto` resolves
      cleanly on boot.
- [x] Killing a loop's goroutine shows up as a flat line on the dashboard, and the
      supervisor brings it back — verified for real (session 7) via a temporary,
      reverted fault injection: `/healthz` showed `copywriter` as
      `last_tick: null, healthy: false` for the ~3.5 minutes the fault was
      active, with the other four loops unaffected throughout, then recovered
      on its own the instant the fault stopped. See the runbook for the full
      before/after `/healthz` output.

Two more things session 7 verified that aren't separate boxes above but are
part of its own done-when list: killing backup access (an unreachable
Litestream target) left `/healthz` green and a reminder still fired mid-outage
— see the runbook. Tunnel reachability (`/healthz` through Cloudflare Access,
`/metrics` refused) was **not** verified — this session had no access to the
real `cloudflared` instance or Cloudflare account; `ops/cloudflared-ingress.md`
has the ingress rule and Access table to apply on the real host, and reachability
needs checking there, not here.

**A few evenings.** The estimate was one or two when this was going to be Python;
D-021 traded some of that for a static binary and a language worth knowing, and
the honest cost is a slower P0. This is also the phase that has to be right, so
it is the wrong one to rush. Everything after it is recoverable.

---

## P1: Conversation

**Goal.** Reminders are created by messaging rather than by SQL.

- Canonical `IncomingMessage` and the inbound half of the transport interface
- Telegram inbound adapter, webhook secret, sender allowlist
- Model client with per-task tier configuration
- Tool catalog: `list_items`, `create_item`, `update_item`, `delete_item`
- Context injection: now, timezone, active items
- Validation layers and the escalation ladder
- `llm_calls` logging
- Confirmation format with the next three concrete timestamps
- `defaults.yaml` vocabulary table

**Exit criteria**

- [x] "Remind me to take vitamins daily at 9am" works end to end — verified
      (session 12) by `cmd/naviseed`'s "conversation ladder" scenario 1: a
      fake tier-1 model returns a `create_item` call for a daily 09:00
      reminder, the item and 30 occurrences are written in one transaction,
      and a confirmation naming the next three timestamps is sent and
      persisted (scenario 6 in the same block: user, assistant, and tool rows
      all present).
- [x] "Remind me to call my grandmother periodically through the week"
      produces a sensible fuzzy schedule with no clarifying question —
      verified (session 12) by scenario 9: a fuzzy `count=3, period=week`
      schedule resolves its window, `days_allowed`, and `min_gap_hours` from
      `defaults.yaml` with no request for clarification (the reply contains
      no `?`), and the confirmation states every inferred field: `Done —
      "call my grandmother" is set. I assumed window 09:00-21:00,
      days_allowed MO,TU,WE,TH,FR,SA,SU, min_gap_hours 20h. Next: ...`.
- [x] "Make it more like five times" resolves against the last touched item —
      the reference-resolution half of this needs a real model and is
      outside what a fake tier-1 server can demonstrate; what session 12
      built and verified (scenario 8) is the plumbing it resolves against:
      `create_item` records `kv.last_touched_item`, and the next turn's
      system prompt names that item on a `Last touched:` line before the
      model is ever asked to resolve anything.
- [x] An invalid schedule triggers retry, then escalation, then a rephrase
      request, and writes nothing — verified (session 7 built the ladder,
      re-confirmed session 12) by scenario 2: an unsatisfiable gap walks all
      four attempts (`llm_calls +4`), the active item count is unchanged, and
      the reply asks for a rephrase.
- [x] `llm_calls` shows a tier-one success rate — `navi_llm_calls_total{task,
      tier, outcome}` has existed since the model client landed; session 12
      adds `ops/grafana/dashboards/navi-llm.json`, a stat panel computing
      `crud` tier-1's success ratio directly from that counter plus a
      breakdown table by task/tier/outcome, so the number is a dashboard
      rather than a query someone has to remember to run.

---

## P2: Resolution

**Goal.** Completions get recorded, so there is data worth charting.

- Resolution endpoints and the full transition table
- Telegram inline keyboards on notifications: Done, Snooze, Skip
- Callback query handling: decode `callback_data`, resolve, `answerCallbackQuery`
  with the outcome, `editMessageText` to fold it into the original message
- Snooze child creation, depth cap, delta resolution
- `chains` view
- Early resolution: `complete` on a `pending` occurrence, cancelling its
  notification
- `resolution_source` tracking
- `bulk_resolve` tool and `store.BulkResolve`, atomic — pulled forward from P3,
  since early resolution from a message needs the agent to resolve something

**Exit criteria**

- [x] Done on the reminder message resolves it in one tap, with no typing and no
      navigating to find the item — verified (session 15) by `cmd/naviseed`'s
      callback section: a real `scheduler.Fire` through the real Telegram
      adapter renders a three-button keyboard whose longest `callback_data` is
      38 of Telegram's 64 bytes, and feeding that payload back through
      `telegram.Inbound.ServeHTTP` leaves the occurrence `completed` with
      `resolution_source = notification`. Snooze takes two taps by design — it
      opens R9's four presets rather than privileging one — and the menu tap
      writes nothing.
- [x] The message updates in place to show the outcome, leaving one message in the
      chat rather than two — verified (session 15) against a fake Bot API that
      records every call: a resolving tap produces exactly
      `answerCallbackQuery` then `editMessageText`, in that order, with **zero**
      `sendMessage` calls and no `reply_markup` on the edit. N6's fallback is
      checked too, by making the fake refuse the edit: a short confirmation is
      sent instead, which costs the second message and is what N6 says a
      transport that cannot edit should do.
- [x] Double-tapping Done does not double-record — verified (session 15): the
      same callback payload twice returns `200` both times, leaves
      `resolved_at` byte for byte unchanged, toasts "Already done.", and leaves
      `navi_occurrence_transitions_total{from="notified",to="completed",source="notification"}`
      reading 1. No deduplication lives in the adapter; this is the state
      machine's second idempotency row and nothing else.
- [x] Snooze creates a child, the chain completes once, the streak survives —
      verified (session 14) by `cmd/naviseed`'s snooze section: a notified row
      snoozed through `POST /api/occurrences/{id}/snooze` keeps its `starts_at`
      byte for byte and gains a `pending`, `is_override` child at
      `snooze_depth 1`; completing that child makes `chains` read
      `was_completed=true, snooze_count=1` from either end of the chain, with
      the parent still `snoozed` and never rewritten. Double-tapping the snooze
      returns the same child rather than a second one, and a real
      `scheduler.Fire` pass afterwards sends the snoozed original zero times.
- [x] Hitting the snooze cap resolves the chain as missed — verified (session
      14) by the same section: a chain walked to `snooze_cap` and asked for one
      more returns `409` `snooze_cap_reached` with `current_state: "missed"`,
      writes no fourth child, and leaves the chain reading `snooze_count=3,
      was_completed=false` with its terminal link `missed`. This is the only
      caller of `missed` in the tree until P3's reconciler.
- [x] "Did my stretching already" at 07:00 cancels the 18:00 notification —
      verified (session 15) by `cmd/naviseed`'s `bulk_resolve` section. The tool
      moved forward from P3 to get here: the P1 catalog could not resolve
      anything, and 06-agent-spec argues directly against adding a
      single-occurrence tool beside it. Three due pending rows resolved in one
      atomic call, then a real `scheduler.Fire` pass sends **zero** — the
      cancellation needs no cancel path, because `ListDueOccurrences` and
      `ClaimOccurrence` both filter `status = 'pending'` and a resolved row has
      already left the fire path by construction (R3). One bad id in a batch
      writes nothing at all.

---

## P3: Reconciliation

**Goal.** The app stops nagging and starts asking.

- `notify_policy` including `silent`
- Reconciler loop, per-item and global timing
- Consolidated check-in composition, with a templated fallback
- `context_ref` on conversation rows so replies are recognised
- `POST /api/occurrences/bulk-resolve`, atomic — the **tool** moved forward to
  P2 (session 15), because the last P2 exit criterion needed the agent to
  resolve an occurrence and a single-occurrence tool is what 06-agent-spec
  argues against. `store.BulkResolve` exists; the HTTP endpoint waits for a
  surface that calls it, which is the web app in P4
- `skipped` distinct from `missed`, with resolution notes
- Grace period, then `missed` assignment
- `pause`, item-scoped and global

**Exit criteria**

- [ ] A silent stretching reminder never pushes but appears in the evening check-in
- [ ] One message covers all outstanding items, not one per item
- [ ] "Stretching and vitamins yes, skipped the walk" resolves all three in one write
- [ ] "Did everything except the walk, I was away" records a skip with a reason
- [ ] Nothing is marked missed before the check-in has asked
- [ ] "Pause everything until Monday" suppresses notifications and reconciliation

---

## P3.5: Goals & Briefing

**Goal.** The app tracks what you're working toward, not just what's scheduled,
and it starts the day by telling you what it should look like.

Numbered out of sequence deliberately — it lands after P3 and before P4, and a
decimal keeps every reference to P4 through P7 elsewhere in this repository
stable rather than triggering a renumbering pass for a phase added later. See
[D-025](08-decisions.md#d-025-goals-and-the-morning-briefing-are-sequenced-after-reconciliation-not-built-alongside-resolution)
for why it waits this long, and [11-goals-spec.md](11-goals-spec.md) for the
full design.

- `goals` and `goal_updates` tables: item-linked and freestanding goals as one
  entity (D-024)
- Tool catalog additions: `create_goal`, `update_goal`, `list_goals`,
  `log_goal_progress`
- Goal evaluation: a sweeper-timed pass, not the clock, assigns `met` or
  `missed` at period end (same principle as K6)
- Velocity for item-linked goals, computed from `chains`, never a stored
  counter
- Morning briefing loop: composed ahead of send time (invariant 1), plain-
  template fallback, no new occurrence kind
- Response tracking for the briefing: `kv.awaiting_response:{date}` and a
  grace-window evaluation, reusing reconciliation's shape
- `persona.md` gains whatever Q-16 resolves to before this phase ships

**Exit criteria**

- [ ] A weekly item-linked goal ("gym four times this week") tracks progress
      from `chains` automatically, with no separate write
- [ ] A freeform goal ("ship the report by Friday") accepts a conversational
      progress update and evaluates `met`/`missed` at period end
- [ ] The morning briefing arrives at the configured time with no model call
      in the firing path, and degrades to a plain summary on generation failure
- [ ] A briefing that gets no reply is detected after its grace window, the
      same shape as an occurrence going `missed`
- [ ] Goal progress and velocity numbers from the agent match the dashboard's
      numbers exactly, same as V6 already requires for item statistics

---

## P4: Interfaces

**Goal.** A screen that is faster than typing.

- `templ` templates and a base layout; HTMX wired to the existing endpoints
- Day view: today's occurrences, one-tap resolution, optimistic updates via Alpine
- PWA manifest and service worker, installable to the home screen
- Calendar view over `/api/occurrences`, colour-coded by item and status
- Statistics: completion rate over time, streaks, median lag, time-of-day heatmap,
  charted with uPlot
- Goal progress and velocity charts, same view, reading the same aggregation
  as `get_stats` (O4, O10)
- `get_stats` tool reading the same `chains` view
- Cloudflare Access on `/app` and `/api`

**Exit criteria**

- [ ] The day view is one tap from the home screen
- [ ] Checking an item off feels instant on mobile data — the row flips before the
      request lands, and reconciles or reverts when it does
- [ ] The calendar shows resolved random times, not ranges
- [ ] The agent's numbers match the dashboard's numbers exactly

---

## P5: Personality

**Goal.** The reminders read like something, and the agent notices things.

- Copywriter loop, two-pass timing
- Context blob assembly from the `chains` view
- `persona.md` mounted and hot-readable
- Tone ladder branching
- Anti-repetition via recent messages
- Proactive triggers: consecutive misses, repeated snoozes, dormancy
- `propose_change` tool
- Weekly digest
- Daily cap on unprompted messages

**Exit criteria**

- [ ] Fourteen consecutive days of the same reminder produce fourteen distinct
      messages
- [ ] A five-miss streak produces a proposal, not another nudge
- [ ] Deleting the model API key still delivers every reminder, as plain titles
- [ ] Editing `persona.md` changes the voice without a rebuild

**Deliberately last.** The copywriter needs completion history to have anything to
say. Running it against an empty dataset produces generic filler, and the
conclusion would be that the feature does not work when the problem was that it
had nothing to work with. Live on P0 through P4 for two weeks first.

---

## P6: Calendar export

**Goal.** Reminders appear in the phone's calendar app.

- `.ics` feed endpoint with a path token
- Recurring `VEVENT` export for `fixed` and `windowed`
- Per-occurrence `VEVENT` export for `fuzzy`
- `STATUS:CANCELLED` for skipped occurrences

**Exit criteria**

- [ ] Google Calendar subscribes and renders correctly
- [ ] Apple Calendar subscribes and renders correctly
- [ ] Recurrence rules survive the round trip rather than exporting as flat events

---

## P7: Events

**Goal.** Calendar events managed the same way as reminders.

- `kind = 'event'` activated, event status set enforced
- `ends_at` populated, durations in the calendar view
- Location and attendees in `attrs`
- Event-aware agent tools and prompt handling
- Conflict detection against existing occurrences
- Google Calendar API two-way sync: OAuth, incremental sync tokens, etag conflict
  resolution
- CalDAV two-way sync for Apple

**Exit criteria**

- [ ] "Book lunch with Sam on Thursday at 1" creates an event with a duration
- [ ] Events created here appear in Google Calendar
- [ ] Events created in Google Calendar appear here
- [ ] Concurrent edits resolve without data loss

**Only after the shape has survived a few months of real use.** Two-way sync is
the largest single piece of work in this document and the easiest to get subtly
wrong, and there is no reason to attempt it before the reminder half has proven
its design.

---

## Not scheduled: a dedicated push transport (T10)

**Goal, if it ever happens.** Done and Snooze on the lock screen, without opening
any app, and reminder priority that breaks through a Focus mode.

This has no phase number because it may never be built. It is not P8 in waiting;
it is a decision that has been deliberately left open, with the evidence to settle
it arriving on its own (D-006).

- ntfy outbound adapter, declaring `supports_native_notification_actions` true
- HMAC action tokens, reusing the scheme recorded in
  [07-api-spec.md](07-api-spec.md#action-tokens-not-present-and-what-would-bring-them-back)
- `/a/{token}` handler and its ingress exception
- First-run generation of the action-token signing key (D10)
- `NOTIFY_TRANSPORT=ntfy`, and nothing else changes — that is the test of whether
  D-006 and D-007 were built honestly

**Decide it from data, not from instinct.** After a month of P4, look at
`resolution_source`:

- Mostly `notification` — the friction is tolerable and the buttons are working
  where they are. Nothing to do.
- Mostly `web` — resolution is happening, but only once something has already
  pulled the user to a screen. That is the case T10 was invented for.
- Mostly `agent` — reminders are being answered conversationally, and a push
  channel with buttons would be solving a problem that is not there.

The reason this is late and optional rather than early and assumed is that
building it first costs a second integration, a fourth authentication mechanism,
and a public unauthenticated route, in exchange for a UX property nobody has
measured yet. The reason it is written down in this much detail anyway is that the
cheapest moment to record how something would be built is while the reasoning for
not building it is still fresh.

There is a second argument for it that has nothing to do with buttons, and it may
turn out to be the stronger one: a single transport means one outage takes
delivery, conversation, and reconciliation together. See the failure-mode table in
[03-architecture.md](03-architecture.md#failure-modes).

---

## Sequencing rationale

The order optimises for two things.

**Risk first.** P0 contains everything that is hard to fix later: the schema,
timezone handling, the materializer, and the delivery guarantee. If any of it is
wrong, finding out in week one is much cheaper than finding out in month three.

**Data before analysis.** P2 and P3 fill the occurrences table. P4 and P5 consume
it. Building a statistics dashboard before there are completions to count, or a
copywriter before there is history to reference, means building against
imagination rather than data.

The one deliberate inversion is that P3 comes before the dashboards, even though
dashboards feel more visible. Reconciliation is what makes the completion data
dense rather than sparse, and sparse data makes both P4 and P5 look broken.

P3.5 is the same rule applied to a phase added later rather than planned from
the start: goal evaluation and a briefing that reports on your routine are
both statements about completion history, and they wait for the same reason
P4 and P5 do. See D-025.
