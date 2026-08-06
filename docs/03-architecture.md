# Architecture

## Shape of the system

Two paths share a database and nothing else.

The **write path** is conversational, latency-tolerant, and model-driven. It turns
natural language into rows.

The **fire path** is a polling loop over those rows. It contains no model call and
no network dependency beyond the notification transport.

Keeping these apart is the reason the system stays reliable while still being an
agent. A model failure degrades the writing experience and the message wording.
It cannot stop a reminder from firing.

```
                    ┌──────────────────────────────────────┐
  phone (chat)      │            navi container            │
      │             │                                      │
      ▼             │  ┌────────────┐                      │
  Telegram ─────────┼─▶│  inbound   │──▶ agent ──▶ tools ──┼──┐
  (conversation     │  │  adapter   │      │               │  │
   role)     ◀──────┼──│            │◀─────┘               │  │
                    │  └────────────┘                      │  │
                    │                                      │  ▼
                    │  ┌────────────┐                   ┌─────────┐
                    │  │materializer│──────────────────▶│ SQLite  │
                    │  ├────────────┤                   │  (WAL)  │
                    │  │ copywriter │──────────────────▶│         │
                    │  ├────────────┤                   └─────────┘
                    │  │ scheduler  │◀─────────────────────┤  ▲
                    │  ├────────────┤                      │  │
                    │  │ reconciler │◀─────────────────────┘  │
                    │  └─────┬──────┘                         │
                    │        │                                │
                    │  ┌─────▼──────┐        ┌────────────┐   │
                    │  │  outbound  │        │ HTTP API   │───┘
                    │  │  adapter   │        │ + web app  │
                    │  └─────┬──────┘        └──────▲─────┘
                    └────────┼──────────────────────┼─────────┘
                             ▼                      │
                        Telegram ──▶ iPhone         │
                    (notification role)     │       │
                                     inline button  │
                                      tap returns   │
                                    via /webhook ───┘
                                                    ▲
                                                    │
                                            browser (PWA day view,
                                            calendar, stats)

  Telegram appears twice because it fills both transport roles. It is one bot and
  one chat; the roles stay separately configured so a dedicated push channel can
  be substituted into the lower one later (T10). A button tap arrives as a callback
  query on the same authenticated webhook that carries conversation, not as a
  separate unauthenticated callback.

  all external access via cloudflared tunnel
  /api and /app behind Cloudflare Access; /webhook/telegram behind a shared secret
             │
             ▼
       Litestream ──▶ Cloudflare R2 (continuous backup)
```

## Processes

One container, one static binary, several goroutines. There is no queue, no worker
pool, and no second replica, because a single user generates a few hundred writes
a day and every added moving part is another thing that can silently stop.

| Task | Interval | Responsibility |
|---|---|---|
| **HTTP API** | n/a | REST endpoints, action callbacks, `.ics` feed, static web app |
| **Inbound adapter** | polling or webhook | Normalizes incoming messages, hands them to the agent |
| **Materializer** | nightly, plus on demand | Expands item schedules into occurrence rows 30 days ahead |
| **Scheduler** | 30s | Claims due occurrences, sends notifications, marks `notified` |
| **Copywriter** | 60s | Generates occurrence message text on a two-pass schedule |
| **Reconciler** | 60s (acts at configured local times) | Sends the daily check-in, then applies `missed` after grace |
| **Sweeper** | hourly | Snooze-cap enforcement, log retention, backfill of missed materialization |

Running these in-process rather than as separate containers is deliberate. They
share the SQLite connection pool, they are all trivially small, and a single
process is one thing to restart and one place to read logs.

The tradeoff is that a crash in one loop must not take down the others:

```go
type Loop struct {
    Name     string
    Interval time.Duration
    Tick     func(context.Context) error
}

func (s *Supervisor) run(ctx context.Context, l Loop) {
    for ctx.Err() == nil {
        if err := s.tickOnce(ctx, l); err != nil {
            s.log.Error("tick failed", "loop", l.Name, "err", err)
        }
        select {
        case <-ctx.Done():
        case <-time.After(l.Interval):
        }
    }
}

// tickOnce recovers panics so one bad occurrence cannot kill the loop,
// and records last_tick for /healthz whether or not the body succeeded.
func (s *Supervisor) tickOnce(ctx context.Context, l Loop) (err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("panic in %s: %v", l.Name, r)
        }
        s.health.Observe(l.Name, time.Now())
    }()
    return l.Tick(ctx)
}
```

Two details are load-bearing. The `recover` is per tick rather than per loop, so a
single malformed row costs one interval instead of the loop. And `last_tick` is
recorded in the deferred function, so a loop that is running but failing still
reports as ticking — `/healthz` distinguishes "stalled" from "erroring", which are
different problems with different fixes.

Cancellation is uniform: every loop takes a `context.Context`, and `SIGTERM`
cancels the root (D12).

## Data flow

### Write path

```
message → inbound adapter → canonical IncomingMessage
        → agent (context injection: now, tz, active items, today's occurrences)
        → model tier 1 → tool call
        → schema validation → semantic validation
        → [fail: retry same tier with error → escalate tier → ask user]
        → transaction: write item, delete future pending occurrences, re-materialize
        → confirmation text with next 3 concrete timestamps
        → outbound via conversation transport
```

### Fire path

```
scheduler tick
  → SELECT occurrences WHERE status='pending' AND starts_at <= now
      AND item.notify_policy = 'at_time' AND item not paused
  → BEGIN IMMEDIATE, claim rows, set status='notified', notified_at=now
  → for each: body = occurrence.message_text OR item.title
  → build actions [Done, Snooze, Skip] as abstract descriptors, rendered
      natively by the adapter — inline keyboard buttons on Telegram
  → outbound via notification transport
```

No model call appears in this path. That is the point.

### Resolve path

```
inline button tap   ┐
web day view tap    ├─→ POST /api/occurrences/{id}/resolve
agent bulk_resolve  ┘        │
                             ▼
                     state machine: is this transition legal?
                             │
                    ┌────────┴────────┐
                  legal            already in
                    │              target state
                    ▼                   │
              write resolution          ▼
              cancel pending      return current state (200)
              notification
```

Three surfaces, one endpoint, one state machine. This is what keeps the statuses
coherent and makes idempotency a property of the design rather than a feature
bolted on per surface.

## Transport abstraction

Two roles, one interface. Both are configured by environment variable and can be
pointed at different adapters.

```go
type Transport interface {
    Name() string
    Capabilities() Capabilities

    // Send returns the transport's own message id.
    Send(ctx context.Context, msg Outbound) (externalID string, err error)

    // Receive streams inbound messages until ctx is cancelled. Transports that
    // poll and transports that are fed by a webhook both satisfy this; the
    // difference is confined to the adapter.
    Receive(ctx context.Context) (<-chan IncomingMessage, error)
}

type Outbound struct {
    Recipient string
    Body      string
    Actions   []Action
    Priority  Priority
    ThreadRef string
}

type Capabilities struct {
    SupportsActions                   bool
    SupportsNativeNotificationActions bool
    SupportsRichText                  bool
    MaxBodyLength                     int
}

type Action struct {
    ID    string // "complete" | "snooze" | "skip"
    Label string
    Arg   string // optional, e.g. a snooze delta
}

type IncomingMessage struct {
    SenderID   string
    Text       string
    Transport  string
    ExternalID string
    ReplyTo    string
    ReceivedAt time.Time
}
```

`Send` takes a struct rather than a parameter list because Go has no default
arguments and most calls set two of the five fields. The zero values are the
defaults.

An `Action` names what it does and nothing about how it travels. *How* an action
becomes tappable belongs entirely to the adapter: Telegram packs the action id and
occurrence id into `callback_data` and receives the tap back on its own webhook,
while a push transport that can only carry a URL would build a signed one instead
(T10, and Q6 applies only there). Putting `URL`, `Method`, and `Headers` on this
struct — as an earlier draft did — pushed one transport's delivery mechanism into
the shared vocabulary and made every caller responsible for signing.

Callers branch on `capabilities`, never on `name`. A transport without action
support renders a numbered plain-text list that the agent parses from the reply,
which is how iMessage would work later.

Configuration:

```
NOTIFY_TRANSPORT=telegram
CHAT_TRANSPORT=telegram
```

Both roles point at the same adapter to start with (D-006). The split is kept
because it is the seam a dedicated push channel drops into later, and because it
costs one environment variable to keep and a refactor to reintroduce.

Telegram declares `supports_actions` true and
`supports_native_notification_actions` false. That second flag currently has no
adapter setting it true, which is the point: the notification-versus-chat-message
distinction is recorded in the capability model rather than in anyone's memory, so
the code that would need to change when T10 lands is already the code reading the
flag.

## Model access

All providers behind one OpenAI-compatible client with a per-tier `base_url` and
key. A single package exposes:

```go
func (c *Client) Complete(
    ctx context.Context,
    task Task,
    messages []Message,
    tools []Tool,
) (Result, error)
```

with configuration mapping each `Task` to an ordered tier list. Swapping a model
is a config edit. See [06-agent-spec.md](06-agent-spec.md) for the routing table
and escalation ladder.

**No LLM SDK and no agent framework.** L5 asks for exactly one thing — an
OpenAI-compatible request with a configurable `base_url` — which is `net/http`
and `encoding/json` in about 150 lines. A framework would add a dependency in
order to hide the tiering and escalation logic, which is the part of this system
worth owning outright.

## Deployment

```yaml
services:
  navi:
    image: navi:latest
    restart: unless-stopped
    volumes:
      - ./data:/data                 # sqlite file, local disk only
      - ./persona.md:/config/persona.md:ro
      - ./defaults.yaml:/config/defaults.yaml:ro
    environment:
      - DATABASE_PATH=/data/navi.db
      - NOTIFY_TRANSPORT=telegram
      - CHAT_TRANSPORT=telegram
      - TELEGRAM_BOT_TOKEN=...
      - TELEGRAM_WEBHOOK_SECRET=...
      - ALLOWED_SENDER_ID=...
      - LITESTREAM_REPLICA_URL=s3://...r2...

  cloudflared:
    # existing service, add an ingress rule for navi:8000

  prometheus:
    # scrapes navi:8000/metrics, 15s interval, 90d retention
  grafana:
    # dashboards in ./grafana/dashboards, provisioned from the repo
```

The image is built from `scratch`, since a `CGO_ENABLED=0` binary needs nothing
underneath it but CA certificates and zoneinfo, and both are embedded — the
latter by importing `time/tzdata`, which matters because a `scratch` image has no
`/usr/share/zoneinfo` and every schedule in this system resolves against an IANA
zone. A missing tzdb would look like correct behaviour until the first DST
boundary.

Litestream runs as the container entrypoint wrapping the app process, which is
the standard pattern and avoids a second service.

The bot token and the webhook secret are borrowed credentials and come from the
environment. The calendar path token is one this deployment owns rather than
borrows, so per D10 it is generated into `/data` on first run when absent. Nothing
that signs or authorizes may have a compiled-in or committed default value: a
default signing key is indistinguishable from no signing key, and it is the
failure that survives being copied to a second machine. The action-token signing
key that an earlier draft generated here is gone with the `/a/*` path; it returns
with T10 if T10 ever lands.

Cloudflare Tunnel ingress:

| Path | Protection |
|---|---|
| `/app/*`, `/api/*` | Cloudflare Access, one-time PIN |
| `/calendar/*.ics` | Long random path token |
| `/webhook/*` | Transport-specific secret |
| `/healthz` | Open |

Every path here is either behind Access, behind a secret, or trivial. Notification
actions do not appear in this table at all, because a button tap arrives as a
callback query on `/webhook/telegram` — already authenticated by the shared secret
and already filtered by the sender allowlist (D8).

This is the part of D-006 that is a straightforward improvement rather than a
trade. An earlier draft carried `/a/{token}`, a session-less public path that
could not sit behind Access because the push client issues those requests with no
browser session, and a signed-token scheme existed to make that path safe. One
public path, one authentication mechanism, and one signing key all disappear. T10
would reintroduce all three, which is a cost that belongs in the decision to add
it.

## Storage

SQLite in WAL mode, through `modernc.org/sqlite` — a pure-Go transpilation of
SQLite rather than a cgo binding. That choice is what satisfies D11: `mattn/go-
sqlite3` is the more common driver and requires cgo, which means a C toolchain in
the build and a dynamically linked binary. The pure-Go driver is measurably
slower under heavy concurrent write load, which is a benchmark this workload never
runs.

The file must live on local disk; SQLite locking is unreliable over NFS and SMB,
and a corrupted database is a worse outcome than any convenience gained.

Concurrency is a single writer (the background loops and API share one process)
with concurrent readers. WAL handles readers during writes without blocking. The
scheduler's claim uses `BEGIN IMMEDIATE` to take the write lock up front rather
than risking a mid-transaction upgrade failure.

Two pools, because Go's `database/sql` will otherwise open several connections and
let two writers collide: one writer handle with `SetMaxOpenConns(1)`, and a
separate read-only pool. Pragmas on open: `journal_mode=WAL`, `busy_timeout=5000`,
`foreign_keys=ON`, `synchronous=NORMAL`.

Queries are hand-written SQL compiled by **sqlc** into typed Go. The DDL in
[04-data-model.md](04-data-model.md) stays the source of truth, which is the whole
reason to prefer this over an ORM: the partial indexes, the recursive-CTE `chains`
view, and `BEGIN IMMEDIATE` are the parts that matter, and all three are things an
ORM abstracts badly or not at all.

**All database access goes through a single repository module.** No SQL in loop
bodies, endpoint handlers, or agent tools — they call named functions that return
models.

This is ordinary structure, and it is also the only hedge this design makes
against a change it has not committed to. If the scope assumption in S1 ever
moves, the migration itself is cheap — `ALTER TABLE ADD COLUMN … DEFAULT` does
not rewrite a table in SQLite. The expensive part of retrofitting any
cross-cutting column is *finding every query*, where a miss is silent and looks
like working code. Confined to one module that is a bounded, reviewable edit.
Scattered across five loops and a dozen handlers it is an audit. The boundary
costs nothing to hold from the first commit and cannot be reconstructed later.

Backup is Litestream replicating to R2 continuously. Recovery is
`litestream restore` plus a container start.

## Observability

`/healthz` answers "is it running". Metrics answer "is it working", which is a
different question and the one this system is actually bad at failing loudly
about. Almost everything here degrades silently by design: a plain-title
notification looks fine, a stalled copywriter looks fine, and a model tier that
has started failing every call looks fine right up until the escalation bill
arrives.

Exported on `/metrics` in Prometheus format, scraped locally, rendered in Grafana:

| Metric | Type | Why it earns its place |
|---|---|---|
| `navi_delivery_latency_seconds` | histogram | Scheduled time to send. Q1 says one minute; this is the only thing that proves it |
| `navi_loop_tick_interval_seconds{loop}` | histogram | A loop that slows before it stalls is the signal `/healthz` gives too late |
| `navi_loop_errors_total{loop}` | counter | Distinguishes erroring from stalled |
| `navi_pending_overdue` | gauge | The one alerting condition |
| `navi_occurrence_transitions_total{from,to,source}` | counter | Completion rate and `resolution_source` as a live series rather than a query |
| `navi_llm_calls_total{task,tier,outcome}` | counter | Tier-one success rate, which the routing table is supposed to be rewritten from |
| `navi_llm_latency_seconds{task,tier}` | histogram | |
| `navi_copywriter_fallback_total` | counter | How often reminders actually go out as plain titles |
| `navi_materializer_horizon_days` | gauge | Catches a thinning horizon before the sweeper does |

The `llm_calls` table and the model metrics overlap deliberately. The table is the
90-day analytical record you write SQL against; the metrics are the live series
you look at. Neither replaces the other, and the table's retention policy is why
the counters exist.

Structured logging via `log/slog` to stdout, JSON in the container. No log
aggregation stack — `docker logs` and a Grafana dashboard is the correct amount of
machinery for one user on one box, and adding Loki here would contradict D-019 for
no benefit.

## Failure modes

Each row states what breaks, what the user notices, and what the system does.

| Failure | User-visible effect | Behaviour |
|---|---|---|
| Model provider down | Reminders arrive as plain titles; agent cannot process new requests | Copywriter gives up after the attempt cap and leaves text null; scheduler is unaffected; inbound messages get an explicit apology rather than silence |
| Copywriter falls behind | Some reminders arrive as plain titles | Fallback in the scheduler, `generation_attempts` bounds the retries |
| Telegram unreachable | No notification arrives and the agent cannot be messaged | Occurrence stays `pending`, retried on the next tick; reconciliation queues its message and sends on recovery; the web app is unaffected and remains the working surface |
| Container restarts | Brief gap | Occurrences overdue under the threshold fire immediately; older ones go to reconciliation rather than firing a backlog |
| Cloudflare Tunnel down | No web app | Local firing continues, and outbound Telegram is unaffected because it dials out. Inbound depends on the adapter mode: webhook delivery stalls until Telegram's retries succeed, `getUpdates` long-polling does not care at all |
| Materializer misses a night | Occurrences thin out beyond the horizon | Hourly sweeper detects a horizon shorter than 25 days and re-runs |
| SQLite file lost | Total | Litestream restore |

The pattern across all of these: the reminder still happens, or the reconciliation
pass catches what was dropped. Reconciliation is not just a UX feature, it is the
backstop for every delivery failure in the table.

With one exception, which D-006 accepted knowingly and which is worth stating
plainly rather than leaving to be discovered. Reconciliation is itself a Telegram
message, so a Telegram outage takes out delivery *and* its backstop together. This
is the only correlated failure in the table. Two things bound it: nothing is
marked `missed` during the outage, because K6 assigns `missed` only after
reconciliation has asked and been ignored, so an unsent check-in produces no false
misses; and the day view keeps working, since it depends on the tunnel rather than
on Telegram. Adding a second transport (T10) would decorrelate this, and that is
the strongest argument in its favour — stronger, arguably, than the lock-screen
buttons it would be adopted for.
