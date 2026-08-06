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

- [ ] A `fixed` daily reminder fires within a minute, every day, for three days
- [ ] A `fuzzy` 3-per-week reminder produces three well-spread occurrences
- [ ] Restarting the container mid-day loses nothing and fires nothing twice
- [ ] `litestream restore` into a clean directory reproduces the database
- [ ] The image is `scratch`, the binary is static, and timezones resolve inside it
- [ ] Killing a loop's goroutine shows up as a flat line on the dashboard, and the
      supervisor brings it back

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

- [ ] "Remind me to take vitamins daily at 9am" works end to end
- [ ] "Remind me to call my grandmother periodically through the week" produces a
      sensible fuzzy schedule with no clarifying question
- [ ] "Make it more like five times" resolves against the last touched item
- [ ] An invalid schedule triggers retry, then escalation, then a rephrase request,
      and writes nothing
- [ ] `llm_calls` shows a tier-one success rate

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

**Exit criteria**

- [ ] Done on the reminder message resolves it in one tap, with no typing and no
      navigating to find the item
- [ ] The message updates in place to show the outcome, leaving one message in the
      chat rather than two
- [ ] Double-tapping Done does not double-record
- [ ] Snooze creates a child, the chain completes once, the streak survives
- [ ] Hitting the snooze cap resolves the chain as missed
- [ ] "Did my stretching already" at 07:00 cancels the 18:00 notification

---

## P3: Reconciliation

**Goal.** The app stops nagging and starts asking.

- `notify_policy` including `silent`
- Reconciler loop, per-item and global timing
- Consolidated check-in composition, with a templated fallback
- `context_ref` on conversation rows so replies are recognised
- `bulk_resolve` tool and endpoint, atomic
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

## P4: Interfaces

**Goal.** A screen that is faster than typing.

- `templ` templates and a base layout; HTMX wired to the existing endpoints
- Day view: today's occurrences, one-tap resolution, optimistic updates via Alpine
- PWA manifest and service worker, installable to the home screen
- Calendar view over `/api/occurrences`, colour-coded by item and status
- Statistics: completion rate over time, streaks, median lag, time-of-day heatmap,
  charted with uPlot
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
