# API Specification

All JSON, all timestamps ISO-8601 UTC with a `Z` suffix. Base path `/api` unless
otherwise noted.

Routed with stdlib `net/http.ServeMux`. Since Go 1.22 it handles method matching
and path wildcards — `mux.HandleFunc("POST /api/occurrences/{id}/resolve", h)` —
which covers every route in this document. A third-party router would buy nothing
here; middleware is ordinary `func(http.Handler) http.Handler`.

## Authentication

Four separate mechanisms, because four callers with genuinely different
constraints hit this service.

| Surface | Paths | Mechanism | Why |
|---|---|---|---|
| Web app and dashboards | `/app/*`, `/api/*` | Cloudflare Access, one-time PIN | Browser sessions, so a real identity layer is free |
| Notification action buttons | `/a/*` | HMAC token in the URL | The ntfy iOS client issues these with no browser session, so Access cannot apply |
| Calendar subscription | `/calendar/{token}.ics` | Long random path token | Google and Apple calendar clients cannot authenticate interactively |
| Transport webhooks | `/webhook/{transport}` | Per-transport shared secret | Provider-defined |
| Health | `/healthz` | Open | Needs to work when everything else does not |
| Metrics | `/metrics` | Not exposed through the tunnel | Scraped by Prometheus on the container network only |

### Action tokens

Notification buttons carry a signed token rather than a session or an API key.

```
token   = base64url(payload) + "." + base64url(hmac_sha256(secret, payload))
payload = {"o": occurrence_id, "a": action, "e": expiry_unix}
```

Properties that matter:

- **Scoped to one occurrence and one action.** A leaked token can complete exactly
  one reminder, which is close to a harmless outcome.
- **Stateless.** No lookup, no revocation list, no storage.
- **Expires in 24 hours.** Long enough for a notification sitting unactioned
  overnight, short enough that revocation is unnecessary.

Tokens appear in the notification payload, which transits ntfy's push
infrastructure. The scoping is what makes that acceptable.

The signing secret and the calendar path token are both generated into the data
directory on first run when absent (D10). Neither has a default. Regenerating the
signing secret invalidates outstanding action tokens, which is bounded by their
24-hour expiry and is the intended way to revoke.

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

### `POST /api/occurrences/{id}/snooze`

```json
{ "delta": "1h" }
```

Returns the new child occurrence. Returns `409` if `snooze_depth` has reached the
item's cap, and resolves the chain as `missed` in that case.

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

### `GET /a/{token}`

Action endpoint for notification buttons. No session. Decodes and verifies the
token, applies the encoded action, returns a minimal HTML confirmation page
because the ntfy client may surface the response.

Deliberately `GET` and not `POST`: the ntfy action configuration is simpler for
GET, and idempotency is guaranteed by the state machine regardless of method,
which is what actually matters here.

Triggers a short confirmation push, because the ntfy iOS client does not dismiss
the originating notification on action tap.

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

## Statistics

### `GET /api/stats/summary?range=week|month|quarter|all`

Completion rate, current and longest streak per item, median lag from notification
to resolution, totals by status.

### `GET /api/stats/timeseries?range=&bucket=day|week`

### `GET /api/stats/heatmap?range=`

Completions bucketed by day of week and hour of day.

All three read the `chains` view, so a snooze chain counts once and the agent's
`get_stats` tool returns numbers identical to the dashboard.

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
allowlisted id, silently and without a reply. Normalizes into `IncomingMessage`
and enqueues for the agent.

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
