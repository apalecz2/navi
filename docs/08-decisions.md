# Decision Records

Each record states the decision, what forced it, and what it costs. The cost
column is the point: every one of these has a downside, and writing it down now
means recognising the symptom later instead of rediscovering the reasoning.

---

## D-001: The LLM is never in the firing path

**Decision.** Reminder text is generated ahead of time and stored on the
occurrence row. The scheduler reads a string from a column and sends it. No model
call happens between "this reminder is due" and "the notification is sent".

**Forced by.** A reminder that depends on a model call succeeding is a reminder
that silently fails when the provider returns a 500, adds latency to a path where
latency is a delivery delay, and costs money on every tick. Personalised copy is
worth having. It is not worth making the core function conditional on a third
party being up.

**Cost.** Generated text can be stale relative to state that changed after
generation. Mitigated by the two-pass copywriter, not eliminated. A message
generated four minutes ahead can still be wrong if something changes in those four
minutes.

---

## D-002: SQLite rather than the existing Postgres

**Decision.** SQLite in WAL mode on local disk, with Litestream replicating to R2.
Accessed through `modernc.org/sqlite`, the pure-Go implementation, rather than a
cgo binding — see D-021 and D11.

**Forced by.** One user, one writer process, a few hundred writes a day. Postgres
buys concurrency control this workload never exercises. SQLite makes backup a file
copy, makes local debugging a scp, and decouples the project from a shared
instance whose schema you would otherwise be reluctant to blow away during
development.

Superseded an earlier decision to reuse the shared Postgres. The reasoning there
was "the backups already exist", which Litestream answers better than pg_dump
does.

**Cost.** No `FOR UPDATE SKIP LOCKED`, so multi-replica is off the table without
rework. The database file must stay off network mounts, since SQLite locking is
unreliable over NFS and SMB. Neither constraint binds at this scale.

The scheduler's `BEGIN IMMEDIATE` claim is the only construct in the design with
no portable equivalent; the Postgres form is `SELECT … FOR UPDATE SKIP LOCKED`
over the same query. Recorded so that a database swap, if one is ever wanted, is
a known quantity rather than a discovery. Everything else is ordinary SQL behind
the repository module described in
[03-architecture.md](03-architecture.md#storage).

---

## D-003: Two tables, definition and materialized occurrence

**Decision.** `items` holds schedules. `occurrences` holds concrete instances,
generated 30 days ahead.

**Forced by.** Three requirements collapse into one mechanism. Random schedules
need their randomness resolved at some point, and resolving it in advance makes it
visible. The calendar view needs a date-range query rather than client-side
recurrence expansion. Statistics need a row per instance to aggregate.

Computing occurrences on demand would satisfy none of these cleanly: random times
would differ on every page load, and there would be nothing to attach a completion
to.

**Cost.** A materializer job that must be kept running, plus a horizon that must
be monitored. The hourly sweeper watches for a horizon under 25 days.

---

## D-004: iCalendar RRULE as the recurrence format

**Decision.** Recurrence expressed as RRULE strings where expressible.

**Forced by.** Google Calendar and CalDAV both consume RRULE, so any other choice
means writing a translation layer the day calendar sync starts. Models emit RRULE
reliably because it is well represented in training data. `rrule-go` expands it,
so no date arithmetic gets hand-written.

**Cost.** RRULE cannot express "three times a week, distributed", which forced the
separate `fuzzy` kind. Fuzzy items export to calendars as individual events rather
than as a recurrence rule, which is a cosmetic loss in the calendar app.

---

## D-005: Randomness resolved at materialization, not at fire time

**Decision.** A `windowed` or `fuzzy` schedule draws its concrete time when the
occurrence row is created.

**Forced by.** A random time that is decided when the clock arrives cannot be
displayed in a calendar, cannot be resolved early, and cannot be reasoned about.
Deciding in advance turns a random reminder into an ordinary scheduled one
everywhere downstream.

**Cost.** Re-materializing reshuffles future times, so a time seen on the calendar
yesterday may differ today. Mitigated by the materializer skipping slots that
already exist, so only genuinely new occurrences get fresh draws.

---

## D-006: Notification and conversation are separate transport roles, on one channel to start

**Decision.** Two transport *roles*, configured independently and both behind one
interface with capability flags. Both point at Telegram to begin with. A dedicated
push channel with native lock-screen actions (T10, ntfy the likely candidate) is
deferred and may never be built.

Supersedes an earlier decision to run ntfy for notifications and Telegram for
conversation from day one. What changed is not the reasoning below — it is the
recognition that the reasoning is a *hypothesis about resolution friction*, and
that the design already contains the instrument to test it.

**Forced by.** Telegram is required regardless, because conversation is the
primary way items get created. Adding a second transport before the first has
been used buys one property — Done and Snooze rendered in the notification itself
rather than in the chat message — at the cost of a second integration, a fourth
authentication mechanism, a signed-token scheme, and the only path in the ingress
table that cannot sit behind Cloudflare Access.

That property may well be worth all of it. The honest position is that nobody
knows yet, and `resolution_source` exists precisely to answer it. Building the
cheap version first and reading the column is a better sequence than building the
expensive version first and never learning whether it was necessary.

Splitting the roles rather than collapsing them is what keeps this reversible.
The interface, the capability flags, and the two environment variables all stay.
Telegram declares `supports_actions` true and
`supports_native_notification_actions` false, which is the exact distinction D-007
was written for — the abstraction earns its keep on day one instead of on day
ninety.

**Cost.** Three things, and they are real.

Resolving means opening Telegram. Inline keyboards render inside the chat message,
not in the notification, and that friction is the thing that leaves a statistics
dashboard empty after a week. iOS quick-reply from the notification is a partial
mitigation — typing "done" into the reply field reaches the agent without opening
the app — but it is typing, not a tap.

Priority collapses to silent versus normal. Telegram has no equivalent of an iOS
interruption level, so nothing breaks through a Focus mode. N7 records what is
lost.

Delivery and conversation now share a failure. Previously an ntfy outage left
conversation working and a Telegram outage left delivery working; now one outage
takes both, and it takes reconciliation with them, since reconciliation is a
Telegram message. The web app is the only surviving surface. This is the sharpest
tension with "degrade to boring" in the document, and it is accepted rather than
resolved — the mitigation is that occurrences stay `pending` and reconciliation
catches up on recovery, which is the same backstop every other row in the
failure-mode table relies on.

**Revisit when** `resolution_source` has a month of data. If notification-sourced
resolutions are rare and web-sourced ones dominate, the friction is real and T10
is worth its cost. If resolution mostly happens by messaging the agent, a push
channel with buttons would have been solving a problem that was not there. See
[Q-15](10-open-questions.md#q-15-whether-a-dedicated-push-transport-is-worth-adding).

---

## D-007: Behaviour branches on transport capability, never on transport name

**Decision.** Adapters declare `supports_actions`,
`supports_native_notification_actions`, and `supports_rich_text`. Calling code
reads those flags.

**Forced by.** The requirement to swap to ntfy, Discord, or iMessage later. An
abstraction that is correct on day one and full of `if transport == "telegram"` on
day ninety has not abstracted anything.

D-006 makes this immediately load-bearing rather than speculative. Telegram fills
both roles while declaring `supports_actions` true and
`supports_native_notification_actions` false, so the distinction the flags encode
is being exercised by the only adapter that exists. Code written against the flags
is code that already works the day a push transport is added; code written against
"is this Telegram" is code that has to be found and rewritten.

**Cost.** Ceremony for one adapter serving two roles, where a direct call would
plainly work today.

---

## D-008: `missed` is assigned after reconciliation, not by the clock

**Decision.** End of day triggers a consolidated check-in. Only after that
check-in goes unanswered for a grace period does anything become `missed`.

**Forced by.** "Missed" should mean "asked and got nothing", not "midnight
passed". The distinction matters because every streak, completion rate, and
copywriter tone decision reads that field, and a reminder completed at 22:00 and
reported at 22:30 is not a miss.

It also composes: silent items and ignored notified items land in the same
message, so one mechanism serves both.

**Cost.** A resolution state that is deliberately unsettled for several hours. The
day view must show "awaiting" distinctly from "missed" during that window.

---

## D-009: Reconciliation is one message, and it is the same code path as batch completion

**Decision.** The nightly check-in covers everything unresolved in a single
message. Its reply is handled by `bulk_resolve`, the same tool that handles an
unprompted "did stretching, vitamins and the walk".

**Forced by.** Per-item nagging is what makes reminder apps get muted. One message
a day is tolerable where six are not. And the natural reply to "which of these got
done?" is exactly the batch-completion format, so building them separately would
mean building the same parser twice.

**Cost.** A single failed reconciliation message means a whole day goes
unreconciled. The sweeper backfills on the next run.

---

## D-010: Snooze creates a child occurrence rather than moving the original

**Decision.** The original is marked `snoozed` and keeps its true timestamp. A
child row is created with an incremented depth and an override flag.

**Forced by.** Mutating `starts_at` destroys the record that something was due at
09:00 and got pushed three times, which is precisely the signal the copywriter and
the proposal logic need. It also silently rewrites the calendar.

**Cost.** Statistics must operate on chains rather than rows, which is the
`chains` view and a recursive CTE. Worth it.

---

## D-011: A snooze chain counts once, and completing any link preserves the streak

**Decision.** If any occurrence in a chain completes, the chain completed.

**Forced by.** Incentives. If snoozing broke a streak, the rational response would
be to ignore the notification rather than snooze it, which produces no data at
all. Making honest snoozing free is what keeps the dataset truthful.

**Cost.** A streak can be preserved by something completed hours late. Acceptable,
because median lag is tracked separately and shows exactly that.

---

## D-012: The agent proposes schedule changes and never applies them

**Decision.** `propose_change` writes nothing. Only an explicit user instruction
routes to `update_item`.

**Forced by.** An assistant that silently moves a reminder because it inferred a
preference is a trust-destroying bug wearing a feature's clothes. Once you cannot
predict when your reminders will fire, the entire system is worthless, and no
amount of good inference compensates.

**Cost.** More turns for changes the agent could have got right unprompted.

---

## D-013: Triggers are deterministic, responses are generative

**Decision.** Code decides when the agent speaks. The model decides what it says,
including saying nothing. Unprompted messages capped at three a day.

**Forced by.** "Have a mind of its own" is the requirement most likely to make the
product worse if unbounded. The value is in varied, context-aware *responses*, not
in unpredictable *timing*. Splitting them keeps the personality and drops the
unpredictability.

**Cost.** The agent cannot notice something genuinely novel and speak up about it,
because nothing triggered. Acceptable: the trigger list is a config change away
from growing.

---

## D-014: One resolution endpoint and one state machine across three surfaces

**Decision.** Notification buttons, the web day view, and the agent all call the
same endpoint and the same transition rules.

**Forced by.** Three surfaces with three implementations means three sets of
status semantics, which diverge, and idempotency has to be solved three times.
Converging them makes idempotency a property of the state machine rather than a
feature per surface.

**Cost.** The endpoint carries three auth modes. Contained, because auth is
resolved before the handler runs.

---

## D-015: The agent never asks a clarifying question about a schedule

**Decision.** Under-specified schedules resolve through a documented defaults
table. The agent states its interpretation, shows three concrete timestamps, and
invites correction.

**Forced by.** The explicit requirement, and a real usability point: a
clarification round-trip on a phone costs more than a wrong guess that is easy to
see and easy to correct. Showing timestamps rather than a schedule description is
what makes the correction easy, because "Tue 2:15pm, Thu 10:40am, Sat 4:05pm" is
checkable at a glance and "three times a week at random times" is not.

**Cost.** Occasionally creating something wrong, which then needs a correction
turn. Net cheaper than always asking.

---

## D-016: Vocabulary defaults live in configuration, not in the prompt

**Decision.** "Periodically", "a couple", "in the morning" resolve through
`defaults.yaml`, read by both the model and the validator.

**Forced by.** Stability. The same phrasing should produce the same schedule every
time, and retuning should be a file edit rather than a prompt-engineering session.
It also means the validator and the model agree by construction.

**Cost.** A config file to maintain, and phrasings outside the table still fall
back to model judgement.

---

## D-017: Generalize to `items` now, build events later

**Decision.** The table is `items` with a `kind` column, occurrences carry
`starts_at` and `ends_at`, and calendar sync columns exist unused from day one.

**Forced by.** The materializer, scheduler, calendar query, agent tools, and
notification path are all shared between reminders and events. Building them
against a table called `reminders` means either a parallel stack later or a
migration through live data. Renaming two tables and adding a nullable column
costs nothing today.

**Cost.** A schema slightly more general than the current feature set needs, and
`kind` checks in code that only ever see one value for a while.

---

## D-018: `.ics` export before real calendar sync

**Decision.** Ship a read-only authenticated iCalendar feed. Defer bidirectional
Google and CalDAV sync.

**Forced by.** The feed is roughly forty lines against an already-materialized
occurrences table and gets reminders visible in the phone calendar app
immediately. Bidirectional sync is OAuth, incremental sync tokens, etag conflict
resolution, deletion tombstones, and iCloud CalDAV, which is a project rather than
a feature.

**Cost.** Read-only, and refresh timing belongs to the calendar client. Google can
lag by hours.

---

## D-019: Background loops in-process, not separate containers

**Decision.** Scheduler, copywriter, reconciler, materializer, and sweeper run as
goroutines in the API process, under a supervisor.

**Forced by.** One user. Each loop is small. A single process is one thing to
restart, one place to read logs, and one SQLite connection pool. Separate
containers would add orchestration for a workload that does not need it.

**Cost.** A crash in one loop must not take the others down, so each tick runs
under a `recover` and a supervisor restarts any goroutine that returns. Scaling
out would require pulling them apart.

---

## D-020: No test suite, but a health endpoint

**Decision.** No automated tests, per the explicit requirement. `/healthz` reports
per-loop last-tick times and overdue pending count.

**Forced by.** The requirement, plus the observation that for this system the
thing worth knowing is not "does the code work" but "is the scheduler still
ticking". A stalled loop is silent, and `pending_overdue` above zero is the one
condition that warrants an alert.

**Cost.** Regressions get found in production, which for a personal reminder app
means a missed reminder. Accepted trade.

---

## D-021: Go rather than Python

**Decision.** Go 1.23+, stdlib `net/http`, `modernc.org/sqlite`, `sqlc` over
hand-written SQL. No web framework, no ORM, no LLM SDK.

**Forced by.** Three things, in order of weight.

The concurrency model in D-019 is the language's central abstraction rather than
something bolted on. Five supervised loops with independent intervals, a
cancellable shutdown, and a crash in one that must not touch the others is a
`context.Context`, a `time.Ticker`, and a `recover` per tick. The Python version
of the same design is hand-written asyncio task supervision, and the copywriter's
concurrent model calls contend on the GIL for no reason.

D11 falls out for free. `CGO_ENABLED=0` with a pure-Go SQLite driver produces one
static binary in a `scratch` image, which is the difference between deploying a
file and deploying an interpreter plus a dependency tree plus a lockfile onto a
home server.

Third, and stated plainly because a decision record that hides its motives is
worth less than one that does not: this is a portfolio project as well as a tool,
and Go is deliberate breadth rather than a fourth variation on languages already
demonstrated elsewhere. That reason is real, it is secondary, and it would not
have been sufficient on its own — it happens to point the same direction as the
first two.

**Cost.** Four, none fatal.

`teambition/rrule-go` is not `python-dateutil`; see [Q-14](10-open-questions.md#q-14-rrule-and-dst-expansion-correctness).
Go's `time` package resolves DST-ambiguous and nonexistent local times silently
and not always the way [05-schedule-spec.md](05-schedule-spec.md#dst) requires, so
that handling is written explicitly instead of inherited. Tool-argument structs
need pointers for every optional field because Go cannot distinguish `0` from
absent, which is more ceremony than Pydantic for the same schema. And the
ecosystem for LLM tooling is thinner — irrelevant here, since L5 asks for one
OpenAI-compatible HTTP client and this design would have hand-rolled that anyway.

---

## D-022: Server-rendered HTML with HTMX, not a single-page application

**Decision.** `a-h/templ` templates rendered server-side, HTMX for interaction,
Alpine.js for local state, uPlot for charts. The PWA manifest and service worker
are hand-written. No React, no build step beyond `templ generate`.

**Forced by.** The actual interaction surface. V1 is a list with checkboxes, V4 is
a month grid backed by one date-range query, and V5 is four charts. Every
resolution already round-trips to the server, because D-014 puts one endpoint and
one state machine behind all three surfaces — so the client holds almost no state
worth managing, and a framework whose value proposition is client state management
is being paid for and not used.

There is also a consistency argument. This repository argues at length for one
process, one database file, and no queue. Serving a React build from it would be
the one place the implementation stopped agreeing with its own reasoning.

**Cost.** V3's optimistic updates are deliberate work — an Alpine directive that
flips the row locally while HTMX reconciles the swap — rather than something a
reactive framework provides by default. Charts need a small library since there is
no charting layer. And offline behaviour beyond the service worker's cache is
genuinely harder than it would be in a SPA, which is acceptable because the day
view is one request and the notification path does not depend on the web app at
all.

---

## D-023: Metrics and dashboards are part of the system, not an add-on

**Decision.** Prometheus metrics on `/metrics`, scraped locally, rendered in
Grafana. The series are specified in
[03-architecture.md](03-architecture.md#observability). `pending_overdue > 0` is
the only alert.

**Forced by.** Every degradation path in this design is deliberately silent. That
is the right behaviour — D-001 and the failure-mode table exist so a model outage
still delivers the reminder — but it means a plain-title notification, a stalled
copywriter, and a model tier failing every call all look approximately fine from
the outside. `/healthz` reports whether loops are ticking and nothing about
whether the work is any good.

Two things this project already committed to are metrics wearing other clothes.
`llm_calls` exists so the routing table can be rewritten from tier-one success
rate rather than intuition. `resolution_source` exists to reveal which surface
actually gets used. Both are questions about a trend, and a table with a 90-day
retention policy answers them worse than a time series does.

**Cost.** Two containers that are not the application, in a document that argues
against extra moving parts. The tension is real and resolves cleanly: they are
sidecars, not components. Nothing in the fire path reads them, scraping is pull-
based so a dead Prometheus cannot block the app, and if both die every reminder
still fires. That is the same degradation rule as everything else here.
