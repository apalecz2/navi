# Navi

A self-hosted, single-user reminder service with a conversational agent front end.
Reminders are created and resolved by messaging the agent in natural language, or by
tapping an action in a phone notification, or by checking a box in a small web app.
The system is built so that calendar events can be added later without restructuring.


## Status

Pre-development. No code written. These documents are the agreed specification.

## Documents

| Doc | Contents |
|---|---|
| [01-requirements.md](01-requirements.md) | Numbered functional and non-functional requirements |
| [02-user-stories.md](02-user-stories.md) | User stories with acceptance criteria |
| [03-architecture.md](03-architecture.md) | Components, processes, data flow, deployment, degradation |
| [04-data-model.md](04-data-model.md) | Schema DDL, status state machines, indexes, retention |
| [05-schedule-spec.md](05-schedule-spec.md) | Schedule kinds, materializer, timezones, edit scope, snooze |
| [06-agent-spec.md](06-agent-spec.md) | Tool catalog, prompts, model routing, escalation, copywriter |
| [07-api-spec.md](07-api-spec.md) | HTTP endpoints, auth modes, action tokens, idempotency |
| [08-decisions.md](08-decisions.md) | Decision records with rationale and consequences |
| [09-roadmap.md](09-roadmap.md) | Phased build plan with exit criteria |
| [10-open-questions.md](10-open-questions.md) | Deferred decisions and known unknowns |

## Glossary

Consistent vocabulary matters more than usual here, because several concepts are
close enough to blur together.

**Item.** The definition of a recurring thing. A reminder or, later, an event.
Holds a title, a schedule, and policy flags. Never holds a timestamp for a
specific day.

**Occurrence.** One materialized instance of an item on a specific date and time.
This is the row everything else operates on: notifications fire against it, you
resolve it, the calendar renders it, the stats aggregate it.

**Materialization.** The job that reads item schedules and generates occurrence
rows ahead of time, currently 30 days out. This is where randomness is resolved
into concrete timestamps.

**Resolution.** Marking an occurrence `completed`, `skipped`, or `missed`. The
term covers all three because they share an endpoint, a state machine, and a
tool.

**Reconciliation.** The nightly pass that gathers everything unresolved for the
day and asks about it in a single message, rather than nagging per item.

**Copywriter.** The job that generates the personalized text attached to an
occurrence shortly before it fires.

**Transport.** A messaging channel adapter. Two roles exist: notification
transport (outbound pushes with action buttons) and conversation transport
(two-way chat with the agent).

## Guiding principles

These are the invariants. Where a later decision conflicts with one of these,
the principle wins or the principle gets explicitly revised.

1. **The LLM is never in the firing path.** A reminder fires because a row in
   SQLite says it is due, not because a model call succeeded. Everything the
   model contributes is generated in advance and stored, and everything degrades
   to a plain title if it is absent.

2. **History is immutable.** Only `pending` occurrences are ever deleted or
   rewritten. Editing a schedule never alters what already happened.

3. **Triggers are deterministic, responses are generative.** Code decides when
   the agent speaks. The model decides what it says, including saying nothing.
   The agent proposes schedule changes and never applies them unilaterally.

4. **One state machine, many surfaces.** Notification buttons, the web app, and
   the agent all converge on the same resolution endpoints and the same
   transition rules. No surface gets its own semantics.

5. **Degrade to boring.** Every enhancement layer has a defined failure mode that
   still delivers the reminder. Losing personality is acceptable. Losing the
   reminder is not.

6. **Build for events, ship reminders.** The schema, materializer, scheduler, and
   agent tools are generic over item kind from day one. Event-specific behaviour
   is deferred, event-shaped data structures are not.
