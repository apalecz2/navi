# Requirements

IDs are stable. Reference them in commits, issues, and when revising this document.

Priority levels:

- **M** (must) required for the system to be usable at all
- **S** (should) required for the system to be good, but the app functions without it
- **L** (later) explicitly deferred, but the design must not preclude it

---

## S. Scope

| ID | Pri | Requirement |
|---|---|---|
| S1 | M | Single user. No multi-tenancy, no user table, no per-user configuration. Deferred rather than rejected: the product today is single-tenant and the commercialization shape, if any, is undecided. See [10-open-questions.md](10-open-questions.md#q-13-commercialization-shape). |
| S2 | M | No automated test suite required. Rough edges are acceptable; unreliable reminder delivery is not. One carve-out is under discussion, in [Q-14](10-open-questions.md#q-14-rrule-and-dst-expansion-correctness). |
| S3 | M | Scope is reminders and, later, calendar events. Not a general-purpose assistant. |
| S4 | L | Calendar events supported as a first-class item kind with duration, sharing all conversational actions with reminders. |

## D. Deployment and infrastructure

| ID | Pri | Requirement |
|---|---|---|
| D1 | M | Runs as Docker Compose services on the existing home server. |
| D2 | M | Exposed through the existing Cloudflare Tunnel and reachable from anywhere. |
| D3 | M | SQLite as the datastore, in WAL mode, on a local volume rather than a network mount. |
| D4 | M | Continuous backup of the SQLite file to Cloudflare R2 via Litestream. |
| D5 | M | Services: `navi` (API plus background loops), `cloudflared` (existing). Optionally `litestream` if not run as the container entrypoint. |
| D6 | M | Single application replica. Background loops run as goroutines in the API process, under a supervisor that restarts any that exit. |
| D7 | M | Cloudflare Access in front of the dashboard and web app, one-time PIN to email. |
| D8 | M | Inbound conversation restricted to a single allowlisted sender ID. Everything else is dropped silently. |
| D9 | S | All secrets supplied by environment variable. No credentials in the image or repository. |
| D10 | S | Secrets the deployment owns rather than borrows — the action-token signing key, the calendar path token — are generated into the data directory on first run when absent. Never a compiled-in or committed default. |
| D11 | S | Ships as a single statically linked binary with no runtime dependencies, built `CGO_ENABLED=0` so the image can be `scratch` or distroless. The SQLite driver must therefore be pure Go. |
| D12 | M | Graceful shutdown: on `SIGTERM` the HTTP server drains, every loop is cancelled through its context, and any in-flight claim transaction commits or rolls back before exit. |

## T. Transport

| ID | Pri | Requirement |
|---|---|---|
| T1 | M | Two transport roles, configured independently: notification (outbound) and conversation (two-way). |
| T2 | M | Both roles implement a common interface, so a channel can be swapped by configuration rather than code change. |
| T3 | M | Outbound interface: `send(recipient, body, actions, priority) -> message_id`, where actions are declared abstractly and rendered natively per channel. |
| T4 | M | Inbound adapters normalize into a canonical message shape regardless of whether the channel polls or receives a webhook. |
| T5 | M | Behaviour branches on declared transport capability flags (`supports_actions`, `supports_native_notification_actions`, `supports_rich_text`), never on transport name. |
| T6 | M | Initial notification transport: Telegram, the same channel as conversation. Both roles are still configured independently, so a dedicated push channel is a configuration change rather than a code change. |
| T7 | M | Initial conversation transport: Telegram. |
| T8 | L | Additional adapters: ntfy, Discord, iMessage. No design change required to add one. |
| T9 | S | A transport without action support falls back to a numbered plain-text list the agent can parse from a reply. |
| T10 | L | A dedicated push transport carrying native lock-screen actions, ntfy being the likely candidate. Deferred and possibly permanent: see [D-006](08-decisions.md#d-006-notification-and-conversation-are-separate-transport-roles-on-one-channel-to-start) and [Q-15](10-open-questions.md#q-15-whether-a-dedicated-push-transport-is-worth-adding). |

## N. Notifications

| ID | Pri | Requirement |
|---|---|---|
| N1 | M | Delivered to iPhone. |
| N2 | M | Notification body is read from a pre-generated string on the occurrence row. No model call occurs at fire time. |
| N3 | M | If that string is null, the plain item title is sent. Delivery never depends on an LLM call succeeding. |
| N4 | M | Notifications carry Done, Snooze, and Skip actions that resolve the occurrence in one tap, with no typing and no navigating to find the item. On Telegram these are inline keyboard buttons attached to the message. |
| N5 | S | Notification priority is per item and maps to the strongest signal the transport offers. On Telegram that is silent versus normal delivery. |
| N6 | S | After an action is tapped, the originating message is edited in place to show the outcome. A transport that cannot edit a message it has already sent sends a short confirmation push instead. |
| N7 | L | Resolution from the lock screen without opening any app, and priority mapped to iOS interruption levels so high-priority reminders break through Focus modes. Both require T10. |

## I. Item model

| ID | Pri | Requirement |
|---|---|---|
| I1 | M | An item has a kind (`reminder`, later `event`), title, optional notes, schedule, timezone, and policy flags. |
| I2 | M | An occurrence is one materialized instance of an item, with a start time and an optional end time. |
| I3 | M | Occurrences are materialized 30 days ahead, and re-materialized when a schedule changes. |
| I4 | M | Kind-specific fields live in a JSON attributes column, not as nullable columns on the main table. |
| I5 | M | Items carry `source`, `external_id`, `etag`, and `last_synced_at` from the start, unused until calendar sync exists. |
| I6 | S | Items can be paused until a date, individually or globally, without deleting or resolving their occurrences one by one. |

## C. Scheduling

| ID | Pri | Requirement |
|---|---|---|
| C1 | M | Four schedule kinds: `one_off`, `fixed` (RRULE plus a fixed time), `windowed` (RRULE plus a random time within a window), `fuzzy` (N times per period, distributed across allowed days). |
| C2 | M | Recurrence expressed as iCalendar RRULE where applicable, because it is what calendar systems consume and what models emit reliably. |
| C3 | M | `fuzzy` schedules honour a minimum gap between occurrences, so "three times this week" does not resolve to three slots in one morning. |
| C4 | M | Random times are resolved once, at materialization, and are therefore concrete and visible in advance. |
| C5 | M | Occurrence timestamps stored in UTC. Each item carries an IANA timezone. |
| C6 | M | Each item has a timezone mode: `fixed` (fire at that wall clock regardless of where the user is) or `floating` (fire at local wall clock wherever the user currently is). Default `floating`. |
| C7 | M | Current device timezone is tracked server-side and updatable by messaging the agent. |
| C8 | M | The scheduler polls the database on a fixed interval. In-memory cron is not the source of truth. |
| C9 | M | On restart, occurrences overdue by less than a threshold fire immediately; older ones pass to the reconciliation path rather than firing a backlog. |

## R. Resolution

| ID | Pri | Requirement |
|---|---|---|
| R1 | M | Occurrence statuses for reminders: `pending`, `notified`, `completed`, `skipped`, `snoozed`, `missed`. |
| R2 | M | `skipped` is distinct from `missed`. A deliberate skip carries a reason and does not damage a streak the way a miss does. |
| R3 | M | An occurrence can be resolved before it fires, so a task done early can be marked complete and its notification cancelled. |
| R4 | M | Resolution is idempotent. Resolving an already-resolved occurrence returns current state rather than erroring or double-recording. |
| R5 | M | Illegal status transitions are rejected by an explicit state machine shared by every surface. |
| R6 | M | Snoozing creates a child occurrence and marks the original `snoozed`. It does not mutate the original timestamp. |
| R7 | M | A snooze chain counts as one occurrence for statistics. If any link completes, the chain is complete and the streak survives. |
| R8 | M | Snooze chains are capped. Past the cap the chain resolves as `missed`. |
| R9 | M | Snooze deltas are presets (10 minutes, 1 hour, tonight, tomorrow), with relative terms resolved against the item's timezone and window rather than by naive arithmetic. |
| R10 | S | Repeated snoozes on one occurrence are treated as a signal that the scheduled time is wrong, and trigger the same proposal path as repeated misses. |

## K. Notification policy and reconciliation

| ID | Pri | Requirement |
|---|---|---|
| K1 | M | Each item has a notification policy: `at_time` (push when due), `silent` (no push, surfaced only if unresolved at reconciliation), `digest` (batched into a scheduled summary). |
| K2 | M | A silent item still generates occurrences, appears in the calendar and the day view, and can be resolved normally. |
| K3 | M | A daily reconciliation pass runs at a configurable local time and gathers everything unresolved for that day. |
| K4 | M | Reconciliation sends one consolidated message covering all unresolved items, not one message per item. |
| K5 | M | Reconciliation covers both silent items and `at_time` items that were notified and ignored. |
| K6 | M | `missed` is assigned only after reconciliation has asked and received no answer within a grace window. It is not assigned by the clock passing midnight. |
| K7 | M | Grace period defaults to end of day in the item's local timezone, and is overridable per item. |
| K8 | S | Reconciliation time is configurable globally and overridable per item. |

## A. Agent

| ID | Pri | Requirement |
|---|---|---|
| A1 | M | Create, modify, and delete items conversationally in natural language, from any device. |
| A2 | M | Tool catalog covers listing, creating, updating, deleting, bulk resolution, snoozing, pausing, statistics, change proposals, and escalation requests. |
| A3 | M | Every turn injects the current datetime, the active timezone, the list of active items, and today's occurrences with their current status. |
| A4 | M | The agent never asks a clarifying question for an under-specified schedule. It applies documented defaults, states the interpretation, and invites correction. |
| A5 | M | Every write is confirmed back in plain language, including the next three concrete occurrence timestamps, because abstract schedule descriptions are hard to verify and timestamps are not. |
| A6 | M | Vocabulary defaults ("periodically", "a couple", "in the morning") are resolved from a configuration table, not re-invented by the model on each request. |
| A7 | M | Deletions require explicit confirmation. |
| A8 | M | A plain-text list of completed items in one message resolves all of them in a single atomic operation, including negations such as "everything except the walk, I was away". |
| A9 | M | Conversation state tracks the most recently touched item so follow-ups like "make it more like five times" resolve without re-identifying it. |
| A10 | M | Conversation history capped at roughly the last 20 messages. |
| A11 | M | All tool arguments are schema-validated and semantically validated before any write. |
| A12 | M | The agent may propose a schedule change but may never apply one unilaterally. |
| A13 | S | Unprompted agent messages are capped per day. |

## G. Message generation

| ID | Pri | Requirement |
|---|---|---|
| G1 | M | Reminder text is generated per occurrence rather than being a static title. |
| G2 | M | Generation is aware of recent completion history, streaks, misses, snoozes, and skip reasons. |
| G3 | M | Two-pass generation: a safety-net pass roughly 30 minutes ahead, and a regeneration pass roughly 4 minutes ahead if relevant state changed. |
| G4 | M | Generation failure is bounded by an attempt counter and falls back to the plain title. |
| G5 | M | Voice is defined in a `persona.md` file mounted into the container and editable without a rebuild. |
| G6 | M | The last few generated messages for an item are passed into the prompt to prevent phrasing loops. |
| G7 | S | Tone strategy branches on state: streak, single miss, repeated misses (shrink the ask), long dormancy (question the schedule rather than nag). |
| G8 | M | Never guilt, never scold. |
| G9 | S | Personality is switched on only after enough completion history exists for it to have something to work with. |

## L. Model routing

| ID | Pri | Requirement |
|---|---|---|
| L1 | M | Model selectable per task through configuration. |
| L2 | M | Ordered tier list per task, with automatic escalation from smaller to larger. |
| L3 | M | Escalation ladder: attempt, retry once at the same tier with the validation error appended, escalate one tier, then ask the user to rephrase. Never write an unvalidated row. |
| L4 | M | Additional escalation triggers: the model calls `request_escalation`, or returns no tool call when a write was expected. |
| L5 | M | All providers accessed through one OpenAI-compatible interface so model choice is configuration, not code. |
| L6 | M | Every model call is logged with task, tier, model, token counts, latency, escalation flag, and reason. |
| L7 | S | Call log has a retention policy. |

## V. Interfaces

| ID | Pri | Requirement |
|---|---|---|
| V1 | M | A day view listing everything due today with one-tap resolution, installable to the phone home screen as a PWA. |
| V2 | M | The day view uses the same resolution endpoints as the notification actions and the agent. |
| V3 | M | Optimistic UI updates in the day view, since it will be used on unreliable mobile connections. |
| V4 | M | A calendar view of upcoming occurrences, backed by a single date-range query, colour-coded by item and status. |
| V5 | M | A statistics view covering completion rate over time, current and longest streak per item, median lag from notification to resolution, and a time-of-day completion heatmap. |
| V6 | S | Statistics shown to the user and statistics available to the agent come from the same aggregation code. |

## X. Calendar integration

| ID | Pri | Requirement |
|---|---|---|
| X1 | S | An authenticated `.ics` feed endpoint exposing materialized occurrences over a date range, subscribable from Google Calendar and Apple Calendar. |
| X2 | S | `fixed` and `windowed` items export as recurring VEVENTs. `fuzzy` items export as individual VEVENTs per occurrence, since the pattern has no iCalendar representation. |
| X3 | L | Bidirectional sync with Google Calendar via its API, and with Apple Calendar via CalDAV. |
| X4 | L | Events support duration, location, and attendees, and are managed through the same conversational tools as reminders. |

## Q. Non-functional

| ID | Pri | Requirement |
|---|---|---|
| Q1 | M | A reminder fires within one minute of its scheduled time under normal operation. |
| Q2 | M | No user-visible latency depends on a model call. |
| Q3 | M | The system survives restart without losing or duplicating pending occurrences. |
| Q4 | M | The system survives a model provider outage with degraded messaging but intact delivery. |
| Q5 | S | Recovery from total host loss is restoring the Litestream replica and starting the container. |
| Q6 | L | Applies only under T10. A transport whose action callbacks arrive unauthenticated must embed signed tokens scoped to a single occurrence and one action, and they must expire. Not required while every callback arrives over an authenticated transport webhook from an allowlisted sender. |
| Q7 | S | Metrics exported in Prometheus format on a scrape endpoint, covering at minimum: delivery latency (scheduled time to send), per-loop tick interval, occurrence status transitions, model tier success and escalation rate, and copywriter fallback rate. |
| Q8 | S | A dashboard renders Q7. `pending_overdue > 0` is the one condition that alerts; everything else in this system degrades visibly on its own. |

---

## Explicitly out of scope

- Multiple users, sharing, or delegation
- Native mobile applications
- Location-based or geofenced triggers
- Voice input
- Automated test suite
- Horizontal scaling, queues, or a separate worker process
