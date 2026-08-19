# API Specification

All JSON, all timestamps ISO-8601 UTC with a `Z` suffix. Base path `/api` unless
otherwise noted.

Routed with stdlib `net/http.ServeMux`. Since Go 1.22 it handles method matching
and path wildcards — `mux.HandleFunc("POST /api/occurrences/{id}/resolve", h)` —
which covers every route in this document. A third-party router would buy nothing
here; middleware is ordinary `func(http.Handler) http.Handler`.

## Authentication

Three separate mechanisms, because three callers with genuinely different
constraints hit this service.

| Surface | Paths | Mechanism | Why |
|---|---|---|---|
| Web app and dashboards | `/app/*`, `/api/*` | Cloudflare Access, one-time PIN | Browser sessions, so a real identity layer is free |
| Calendar subscription | `/calendar/{token}.ics` | Long random path token | Google and Apple calendar clients cannot authenticate interactively |
| Transport webhooks | `/webhook/{transport}` | Per-transport shared secret, plus the sender allowlist | Provider-defined |
| Health | `/healthz` | Open | Needs to work when everything else does not |
| Metrics | `/metrics` | Not exposed through the tunnel | Scraped by Prometheus on the container network only |

Notification action buttons do not get their own row. A tap on an inline keyboard
button arrives as a callback query on `/webhook/telegram`, so it is authenticated
by the webhook secret and filtered by the allowlist before anything else looks at
it. There is no session-less public action path in this service.

The calendar path token is generated into the data directory on first run when
absent (D10) and has no default.

### Action tokens: not present, and what would bring them back

An earlier draft signed a token into every notification, because the push client
issued action requests directly and with no session:

```
token   = base64url(payload) + "." + base64url(hmac_sha256(secret, payload))
payload = {"o": occurrence_id, "a": action, "e": expiry_unix}
```

Scoped to one occurrence and one action, stateless, 24-hour expiry. It is recorded
here because the scheme is correct and should be reused rather than redesigned if
T10 lands — a transport that can only carry a URL has no other way to authorize a
tap, and Q6 exists for exactly that case.

It is not built now because Telegram does not need it. `callback_data` carries the
occurrence id and action over a channel this service already authenticates, and a
64-byte budget holds a 26-character ULID and an action name comfortably. Building
the token scheme anyway would mean maintaining a signing key, a fourth
authentication mechanism, and a public unauthenticated route in order to serve no
caller.

## Idempotency

Not a separate mechanism. It falls out of the status state machine in
[04-data-model.md](04-data-model.md#status-state-machines).

| Situation | Response |
|---|---|
| Transition is legal | `200`, new state |
| Occurrence already in the requested terminal state | `200`, current state, nothing written |
| Occurrence in a different terminal state | `409`, current state |
| Transition is illegal from the current state | `409`, current state |

A double-tapped notification button and a mobile retry both land in row two. No
idempotency keys, no request deduplication table.

---

## Occurrences

### `GET /api/today`

Everything due today, in local time, with current status. Backs the day view.

```json
{
  "date": "2026-08-05",
  "timezone": "America/Toronto",
  "occurrences": [
    {
      "id": "occ_01H...",
      "item_id": "itm_01H...",
      "title": "vitamins",
      "starts_at": "2026-08-05T13:00:00Z",
      "starts_at_local": "09:00",
      "status": "completed",
      "resolved_at": "2026-08-05T13:12:04Z",
      "resolution_source": "web",
      "notify_policy": "at_time",
      "priority": 3,
      "snooze_depth": 0
    }
  ],
  "counts": { "total": 6, "resolved": 3, "outstanding": 3 }
}
```

### `GET /api/occurrences?from=&to=&status=&item_id=`

Date-range query backing the calendar view. `from` and `to` are ISO dates,
inclusive. Capped at 400 days.

### `POST /api/occurrences/{id}/resolve`

The single resolution endpoint. Used by the web app and, internally, by the
agent's `bulk_resolve`.

```json
{ "status": "completed", "note": null, "source": "web" }
```

`status` is one of `completed`, `skipped`, `missed`.

Side effects on success:

- Writes `status`, `resolved_at`, `resolution_note`, `resolution_source`
- Cancels a pending notification if the occurrence had not yet fired
- Rolls the snooze chain up if the occurrence has a parent

The roll-up is a **read** over the `chains` view, taken inside the same
transaction as the write and returned as a `chain` object on the response. It
never writes to ancestors: they are `snoozed`, which is terminal, and history is
immutable. It is present on every response and not only on a child's, because an
occurrence that was never snoozed is a chain of one — reporting it that way is
what keeps "a chain counts once" a single rule rather than two.

### `POST /api/occurrences/{id}/snooze`

```json
{ "delta": "1h", "source": "web" }
```

`delta` is one of `10m`, `1h`, `tonight`, `tomorrow` (R9). `source` is required
and takes the same four values `resolve` does: the parent's `resolution_source`
column and the `source` label on
`navi_occurrence_transitions_total{from,to,source}` both need it, and neither can
guess. There is no `note` — a snooze is not a resolution anyone attaches a reason
to.

Returns `200` with the child, the parent, and the chain roll-up:

```json
{
  "occurrence": {
    "id": "occ_01H...", "item_id": "itm_01H...",
    "starts_at": "2026-08-05T19:00:00Z", "status": "pending",
    "is_override": true, "parent_occurrence_id": "occ_01G...", "snooze_depth": 1
  },
  "parent": {
    "id": "occ_01G...", "starts_at": "2026-08-05T13:00:00Z",
    "status": "snoozed", "resolved_at": "2026-08-05T18:02:11Z",
    "resolution_source": "web"
  },
  "chain": {
    "root_id": "occ_01G...", "scheduled_at": "2026-08-05T13:00:00Z",
    "snooze_count": 1, "was_completed": false, "completed_at": null
  }
}
```

The parent is in the response because R6's whole point is that its `starts_at`
did not move, and a body carrying only the child cannot show that. The chain is
D-011 made reportable, so no surface has to walk `parent_occurrence_id` for
itself.

Re-snoozing an already-`snoozed` occurrence is the idempotency table's second
row: `200`, nothing written, and the **same** child returned rather than a
second one minted. Snoozing a `pending` occurrence is a `409` — `notified →
snoozed` is the only edge the transition table has, so a reminder that has not
fired has nothing to be pushed back from.

Returns `409` if `snooze_depth` has reached the item's cap, and resolves the
chain as `missed` in that case — the write happens, and `current_state` reports
it:

```json
{
  "error": "snooze_cap_reached",
  "message": "domain: snooze cap reached: depth 3 of 3",
  "current_state": "missed"
}
```

This is the only path in the service that assigns `missed` before P3's
reconciler exists, and it is the one [04-data-model.md](04-data-model.md#streaks-and-snooze-chains)
blesses by name: "a chain is `missed` if the terminal link is `missed`,
including snooze-cap exhaustion."

### `POST /api/occurrences/bulk-resolve`

Atomic. All or nothing. Backs the agent's `bulk_resolve` tool.

```json
{
  "resolutions": [
    { "occurrence_id": "occ_01H...", "status": "completed" },
    { "occurrence_id": "occ_01H...", "status": "completed" },
    { "occurrence_id": "occ_01H...", "status": "skipped", "note": "on vacation" }
  ]
}
```

Response includes per-item outcomes and a summary. If any occurrence id is
invalid, nothing is written and the response identifies which one.

### Notification button taps

Not an HTTP route on this service. A tap on Done, Snooze, or Skip arrives as a
Telegram callback query on `/webhook/telegram`, and the adapter turns it into the
same internal resolve call the web app reaches through
`POST /api/occurrences/{id}/resolve`. Same endpoint, same state machine, same
transition rules — D-014 is satisfied by one surface fewer, not by a special case.

The adapter's obligations on a tap, in order:

1. Verify the webhook secret and the sender allowlist, as with any inbound update
2. Decode `callback_data` into an occurrence id and an action
3. Resolve, taking whatever the state machine returns
4. `answerCallbackQuery` to clear the client's spinner, with the outcome as the
   toast text — including on a `409`, where the toast reports the current state
5. `editMessageText` to fold the outcome into the original message and drop the
   keyboard

Step five is why N6 changed. The original message is edited rather than followed by
a confirmation push, so a resolved reminder leaves one message in the chat instead
of two. A transport that cannot edit a message it has already sent falls back to
sending the short confirmation, which is what N6 now says.

Double-tapping is handled by the state machine exactly as it is everywhere else,
so no deduplication lives in the adapter. The keyboard is usually gone by the
second tap; when it is not, the second tap is a `200` no-op or a `409` and the
toast says so.

---

## Items

### `GET /api/items?filter=active|all|paused`

### `POST /api/items`

```json
{
  "title": "evening walk",
  "kind": "reminder",
  "schedule": {
    "kind": "windowed",
    "rrule": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR",
    "window": ["17:00", "21:00"]
  },
  "tz": "America/Toronto",
  "tz_mode": "floating",
  "notify_policy": "at_time",
  "priority": 3
}
```

Materializes synchronously and returns `next_occurrences`, the first three
concrete timestamps. This is what the agent's confirmation is built from.

`400` on validation failure, with the specific rule that failed named in the body,
because that text is fed back into the escalation ladder.

### `PATCH /api/items/{id}`

```json
{
  "scope": "future_all",
  "changes": { "schedule": { "kind": "fixed", "rrule": "FREQ=DAILY", "at": "20:00" } }
}
```

`scope` is `future_all`, `from_date` (with `from_date`), or `single` (with
`occurrence_id`). Re-materializes only if a schedule-affecting field changed.
Returns `next_occurrences`.

### `DELETE /api/items/{id}`

Sets `archived_at`, deletes `pending` occurrences, retains resolved history.

### `POST /api/items/{id}/pause` and `POST /api/pause`

```json
{ "until": "2026-08-12" }
```

Item-scoped and global respectively.

---

## Goals

**Status: specified, not built** — P3.5, see [11-goals-spec.md](11-goals-spec.md).

### `GET /api/goals?filter=active|all`

### `POST /api/goals`

```json
{
  "title": "gym four times this week",
  "period_kind": "week",
  "item_id": "itm_01H...",
  "target_count": 4
}
```

`item_id` and `target_count` are set together for an item-linked goal, and both
omitted for a freestanding one. `400` on validation failure, same shape as
`POST /api/items` — the specific rule that failed, named in the body, because
that text is what the escalation ladder reads.

### `PATCH /api/goals/{id}`

Title, period bounds, or status (`abandoned` only — `met`/`missed` are set only
by evaluation, never by direct write).

### `POST /api/goals/{id}/progress`

Freestanding goals only. `409` on an item-linked goal — its progress has no
write path, it is a read over `chains`.

```json
{ "progress_pct": 60, "note": "first draft done" }
```

Appends a `goal_updates` row. At least one of `progress_pct` or `note` is
required.

### `GET /api/goals/{id}/progress`

The full `goal_updates` history for a freestanding goal, or the `chains`-derived
progress series for an item-linked one — same response shape either way, so a
chart component does not need to know which kind of goal it is rendering.

---

## Statistics

### `GET /api/stats/summary?range=week|month|quarter|all`

Completion rate, current and longest streak per item, median lag from notification
to resolution, totals by status. Once P3.5 lands, includes goal rows: progress
toward target and velocity for item-linked goals, latest `progress_pct` for
freestanding ones.

### `GET /api/stats/timeseries?range=&bucket=day|week`

### `GET /api/stats/heatmap?range=`

Completions bucketed by day of week and hour of day.

All three read the `chains` view (and, once P3.5 lands, `goals`/`goal_updates`
alongside it), so a snooze chain counts once, a goal's numbers are computed the
same way regardless of caller, and the agent's `get_stats` tool returns numbers
identical to the dashboard.

---

## Calendar

### `GET /calendar/{token}.ics?from=&to=`

iCalendar feed of materialized occurrences. Defaults to 30 days back and 90 days
forward.

- `fixed` and `windowed` items emit recurring `VEVENT`s with their `RRULE`
- `fuzzy` items emit one standalone `VEVENT` per occurrence, since the pattern has
  no iCalendar representation
- Reminders emit as zero-duration events
- `STATUS:CANCELLED` for skipped occurrences

Includes `X-PUBLISHED-TTL:PT1H`, which Google and Apple treat as a hint rather
than an instruction. Refresh timing is not under this service's control.

---

## Webhooks and health

### `POST /webhook/telegram`

Verified against a per-transport secret. Drops anything whose sender is not the
allowlisted id, silently and without a reply.

Two kinds of update arrive here and they diverge immediately after that check. A
message normalizes into `IncomingMessage` and is enqueued for the agent. A
callback query is a button tap and never reaches the agent at all — it decodes to
an occurrence and an action and goes straight to the resolve path, because a tap
is an instruction that has already been unambiguously expressed and running a
model over it would add latency, cost, and a failure mode to a decision with
nothing left to interpret.

The adapter may instead run in `getUpdates` long-polling mode, which needs no
inbound route and no tunnel ingress. `Receive` looks identical to the rest of the
system either way; T4 is the requirement that makes this a configuration detail
rather than a design one.

### `GET /healthz`

```json
{
  "status": "ok",
  "db": "ok",
  "loops": {
    "scheduler":   { "last_tick": "2026-08-05T18:42:03Z", "healthy": true },
    "copywriter":  { "last_tick": "2026-08-05T18:42:00Z", "healthy": true },
    "reconciler":  { "last_tick": "2026-08-05T18:42:00Z", "healthy": true },
    "materializer":{ "last_run":  "2026-08-05T04:00:00Z", "healthy": true }
  },
  "horizon_days": 30,
  "pending_overdue": 0
}
```

`pending_overdue` above zero means the scheduler has stalled, which is the one
failure worth alerting on. Everything else in this service degrades visibly on its
own.

### `GET /metrics`

Prometheus exposition format, the series listed in
[03-architecture.md](03-architecture.md#observability). Bound to the container
network and deliberately absent from the Cloudflare Tunnel ingress table: it
carries no secrets but it does describe usage patterns in detail, and there is no
reason for it to leave the host.

`/healthz` and `/metrics` answer different questions. `/healthz` is a liveness
check for a human or a restart policy. `/metrics` is the time series that shows a
loop slowing down before it stops, which is the failure this system is otherwise
silent about.

---

## Error format

```json
{
  "error": "validation_failed",
  "message": "min_gap_hours 8 cannot be satisfied with count 5 over period 'day'",
  "field": "schedule.min_gap_hours"
}
```

The `message` field is written to be fed back to a model verbatim during
escalation, so it names the rule and the offending values rather than saying
"invalid schedule".
