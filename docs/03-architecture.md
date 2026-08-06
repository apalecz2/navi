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
   transport)◀──────┼──│            │◀─────┘               │  │
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
                        ntfy ──▶ iPhone ────────────┘
                     (notification         action button
                       transport)          HTTP callback
                                                    ▲
                                                    │
                                            browser (PWA day view,
                                            calendar, stats)

  all external access via cloudflared tunnel
  /api and /app behind Cloudflare Access; /a/{token} behind HMAC
             │
             ▼
       Litestream ──▶ Cloudflare R2 (continuous backup)
```

## Processes

One container, one process, several asyncio tasks. There is no queue, no worker
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

The tradeoff is that a crash in one task must not take down the others. Each loop
wraps its body in a try/except that logs and continues, and the loop supervisor
restarts any task that exits.

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
  → build actions [Done, Snooze, Skip] with HMAC tokens
  → outbound via notification transport
```

No model call appears in this path. That is the point.

### Resolve path

```
ntfy action button  ┐
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

```python
class Transport(Protocol):
    name: str
    capabilities: Capabilities

    async def send(
        self,
        recipient: str,
        body: str,
        actions: list[Action] = (),
        priority: Priority = Priority.NORMAL,
        thread_ref: str | None = None,
    ) -> str: ...          # returns external message id

    async def receive(self) -> AsyncIterator[IncomingMessage]: ...


@dataclass
class Capabilities:
    supports_actions: bool
    supports_native_notification_actions: bool
    supports_rich_text: bool
    max_body_length: int


@dataclass
class Action:
    id: str                # "complete" | "snooze" | "skip"
    label: str
    url: str
    method: str = "POST"
    headers: dict = field(default_factory=dict)


@dataclass
class IncomingMessage:
    sender_id: str
    text: str
    transport: str
    external_id: str
    reply_to: str | None
    received_at: datetime
```

Callers branch on `capabilities`, never on `name`. A transport without action
support renders a numbered plain-text list that the agent parses from the reply,
which is how iMessage would work later.

Configuration:

```
NOTIFY_TRANSPORT=ntfy
CHAT_TRANSPORT=telegram
```

Splitting the roles is what lets ntfy handle notifications, where its native iOS
action buttons are the whole reason it was chosen, while Telegram handles
conversation, where a real chat UI matters.

## Model access

All providers behind one OpenAI-compatible client with a per-tier `base_url` and
key. A single module exposes:

```python
async def complete(task: Task, messages, tools=None) -> Result
```

with a configuration dict mapping each `Task` to an ordered tier list. Swapping a
model is a config edit. See [06-agent-spec.md](06-agent-spec.md) for the routing
table and escalation ladder.

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
      - NOTIFY_TRANSPORT=ntfy
      - CHAT_TRANSPORT=telegram
      - ALLOWED_SENDER_ID=...
      - ACTION_TOKEN_SECRET=...
      - LITESTREAM_REPLICA_URL=s3://...r2...

  cloudflared:
    # existing service, add an ingress rule for navi:8000
```

Litestream runs as the container entrypoint wrapping the app process, which is
the standard pattern and avoids a second service.

Cloudflare Tunnel ingress:

| Path | Protection |
|---|---|
| `/app/*`, `/api/*` | Cloudflare Access, one-time PIN |
| `/a/*` | HMAC action token in the request, no session |
| `/calendar/*.ics` | Long random path token |
| `/webhook/*` | Transport-specific secret |
| `/healthz` | Open |

`/a/*` cannot sit behind Cloudflare Access, because the ntfy client issues those
requests with no browser session. That is why action tokens are signed, scoped to
one occurrence and one action, and short-lived.

## Storage

SQLite in WAL mode. The file must live on local disk; SQLite locking is unreliable
over NFS and SMB, and a corrupted database is a worse outcome than any convenience
gained.

Concurrency is a single writer (the background loops and API share one process)
with concurrent readers. WAL handles readers during writes without blocking. The
scheduler's claim uses `BEGIN IMMEDIATE` to take the write lock up front rather
than risking a mid-transaction upgrade failure.

Backup is Litestream replicating to R2 continuously. Recovery is
`litestream restore` plus a container start.

## Failure modes

Each row states what breaks, what the user notices, and what the system does.

| Failure | User-visible effect | Behaviour |
|---|---|---|
| Model provider down | Reminders arrive as plain titles; agent cannot process new requests | Copywriter gives up after the attempt cap and leaves text null; scheduler is unaffected; inbound messages get an explicit apology rather than silence |
| Copywriter falls behind | Some reminders arrive as plain titles | Fallback in the scheduler, `generation_attempts` bounds the retries |
| ntfy unreachable | No notification arrives | Occurrence stays `pending`, retried on the next tick, and picked up by reconciliation regardless |
| Telegram unreachable | Cannot message the agent | Notifications and web app unaffected; reconciliation queues its message and sends on recovery |
| Container restarts | Brief gap | Occurrences overdue under the threshold fire immediately; older ones go to reconciliation rather than firing a backlog |
| Cloudflare Tunnel down | No web app, no action buttons | Local firing continues; resolution catches up via reconciliation |
| Materializer misses a night | Occurrences thin out beyond the horizon | Hourly sweeper detects a horizon shorter than 25 days and re-runs |
| SQLite file lost | Total | Litestream restore |

The pattern across all of these: the reminder still happens, or the reconciliation
pass catches what was dropped. Reconciliation is not just a UX feature, it is the
backstop for every delivery failure in the table.
