# Open Questions

Things not decided, deliberately deferred, or likely to need revisiting once the
system has been used. Each notes when it needs an answer, so nothing blocks P0
that does not have to.

---

## Needs an answer before P0

### Q-1: Materialization horizon

30 days is a guess. It affects how far the calendar can look ahead, how much a
re-materialization reshuffles, and how quickly a schedule change becomes visible.

A longer horizon means more churn on every edit. A shorter one means the calendar
runs out. 30 days is probably right for reminders and probably too short for
events later, so this may end up being per-kind.

**Resolve by:** picking 30 and revisiting after a month of using the calendar view.

### Q-2: Restart recovery threshold

Occurrences overdue by less than some threshold fire immediately on startup;
older ones go to reconciliation. 30 minutes was the working figure.

Too long and a two-hour outage dumps a batch of stale notifications. Too short and
a five-minute restart silently swallows a reminder. Reconciliation catches
anything dropped, which argues for a shorter threshold than instinct suggests.

**Resolve by:** starting at 15 minutes, since reconciliation is the safety net.

### Q-3: Time rounding granularity

Random times round to 5 minutes so they read as chosen rather than generated.
Whether that is the right granularity, and whether some items want unrounded
times, is untested.

### Q-14: RRULE and DST expansion correctness

Numbered out of sequence because IDs are stable and this arrived with D-021.

The Python version of this spec leaned on `python-dateutil`, which has had twenty
years of edge cases beaten out of it. `teambition/rrule-go` is well regarded and
has had rather fewer. Separately, Go's `time.Date` resolves nonexistent and
ambiguous local times silently, and not always the way
[05-schedule-spec.md](05-schedule-spec.md#dst) requires.

Three specific behaviours are unverified: weekly expansion spanning a DST
boundary, `BYDAY` with a negative index (`-1SU` for "last Sunday"), and the
explicit fold handling this spec adds on top of the stdlib.

**This is the one place S2 is under discussion,** and the argument is narrower
than "the project should have tests." Every other component in this system fails
visibly: a stalled loop shows on the dashboard, a bad tool call is caught by the
validator, a model outage sends the plain title. A materializer that places an
occurrence an hour off twice a year fails *invisibly and rarely*, which is the
one shape hand-verification is structurally bad at — you cannot notice in March
that November will be wrong.

**Leaning:** a single table-driven test over the expansion function, maybe forty
lines, covering the three cases above plus the two DST examples already written
out in the schedule spec. Go ships the runner, so this costs a file and no
dependency. Not a suite, not a framework, not a coverage target — and explicitly
not a precedent for testing anything else here.

**Resolve by:** writing those cases during P0, when the materializer is being
built and the expected values are already in your head. If `rrule-go` passes all
three on the first run, the file stays anyway; it is cheaper to keep than to
reconstruct the reasoning later.

**Resolved, P0 session 4.** `internal/materializer/expand_test.go`, eleven cases
in two tables, all passing on the first run.

`rrule-go` was fine, and it was also handed the easier job: the materializer
expands rules in UTC for dates only, so weekly-across-a-boundary and `BYDAY=-1SU`
were verified over an expander that never sees a timezone.

The stdlib was not fine, and worse than this question assumed. `time.Date`
resolves 02:30 on a spring-forward day in `America/New_York` to **01:30 EST** —
an hour before what was asked for, on the far side of the transition — not
forward as [05-schedule-spec.md](05-schedule-spec.md#dst) had recorded. The
"matches rule one by accident" line has been corrected there. Both edge cases are
now decided explicitly via `time.ZoneBounds`, in `internal/schedule/dst.go`, and
the file stays as the leaning said it would.

The carve-out ends here. It bought one file, no dependency, and no precedent.

---

## Needs an answer before P3

### Q-4: What reconciliation does with a partially-answered check-in

If the check-in asks about three items and the reply covers two, the third is
still unresolved. Options: ask again immediately, wait for the grace period and
mark it missed, or ask once more the following evening.

Asking again immediately risks turning one message into a conversation the user
did not want. Silence risks a miss that was really an oversight.

**Leaning:** resolve what was answered, say nothing about the rest, let grace
handle it. Revisit if it turns out to produce false misses.

### Q-5: Whether `digest` notification policy is worth building

It is in the requirements as a third policy but has no user story driving it. It
may turn out that `at_time` and `silent` cover everything, with reconciliation
serving the role a digest would.

**Leaning:** defer until something actually wants it. Building it speculatively
adds a code path with no user.

### Q-6: Reconciliation when the day had no outstanding items

Say nothing, or send a short acknowledgement? Silence is cleaner. A daily "all
done" message is the kind of thing that is pleasant for a week and then noise.

**Leaning:** silence, with the completion visible in the day view.

---

## Needs an answer before P5

### Q-7: How long "enough history" is

G9 says personality switches on once there is history to work with. Two weeks was
the working figure, but the real threshold is probably a number of resolved
occurrences per item rather than a duration, since a weekly reminder accumulates
history far more slowly than a daily one.

**Leaning:** per-item, roughly 10 resolved occurrences, falling back to the plain
title before that.

### Q-8: Whether generated messages should be reviewable

The two-pass copywriter means text exists on the row before it is sent, so the day
view could show upcoming messages and offer a regenerate button, or a thumbs
up/down that feeds examples into `persona.md`.

That closed loop is appealing and is also exactly the kind of feature that sounds
better than it is used. Worth building only if the voice turns out to be hard to
tune by editing the persona file directly.

### Q-9: Whether the weekly digest earns its place

A Sunday evening summary with statistics and an observation is in the proactive
trigger table. Whether it gets read, or muted along with everything else, is
unknown. It is the easiest thing in P5 to remove.

---

## Needs an answer after P4 has run for a month

### Q-15: Whether a dedicated push transport is worth adding

Numbered out of sequence because IDs are stable and this arrived with the revision
of D-006.

Telegram fills both transport roles. Its inline keyboards render inside the chat
message rather than in the notification, so resolving means opening Telegram. The
open question is whether that friction matters enough to justify T10: a second
integration, a fourth authentication mechanism, a signed-token scheme, and a
public route that cannot sit behind Cloudflare Access.

The earlier version of this specification answered yes in advance and built ntfy
from day one. That answer may still be right. What was wrong was answering it
before there was any evidence, when `resolution_source` had been put in the schema
specifically to produce that evidence and costs nothing to read.

Three outcomes and what each implies:

| `resolution_source` after a month | Reading | Action |
|---|---|---|
| Mostly `notification` | The buttons are being used where they are | Nothing |
| Mostly `web` | Resolution happens, but only once a screen has been opened anyway | T10 is worth its cost |
| Mostly `agent` | Reminders get answered by replying | T10 solves a problem that is not there |

A fourth outcome is worth naming because it does not appear in the column at all:
if occurrences are largely resolved by *reconciliation* rather than in the moment,
the notification is being ignored regardless of what it carries, and no button
placement fixes that. That is the case where the answer is fewer reminders, as the
first known unknown below already says.

**Watch for the confound.** iOS quick-reply lets a "done" be typed straight from
the Telegram notification, which lands as `agent` rather than `notification`. A
month of mostly-`agent` therefore has two readings — reminders being answered
conversationally at leisure, or the lock screen already working well enough
through a different door. Separating them means looking at lag from `notified_at`
to `resolved_at`, not just at the source: quick-replies cluster within seconds of
the notification, considered replies do not.

**Resolve by:** running P0 through P4 and reading the column. There is also a
second argument for T10 that is independent of friction — it decorrelates the
single-transport failure in
[03-architecture.md](03-architecture.md#failure-modes) — and if that outage is
ever actually experienced, it may settle this on its own.

---

## Needs an answer before P7

### Q-10: Conflict resolution strategy for two-way calendar sync

The genuinely hard problem in this document. Last-write-wins is simple and loses
data. Field-level merge is correct and involved. Etag comparison detects conflicts
but does not resolve them.

For a single user editing from two places, last-write-wins with a conflict log may
be enough, since the user can see what was overwritten and fix it. This is worth
deciding properly rather than by default.

### Q-11: Whether events belong in this system at all

The design supports them, which was the point. Whether they should live here
rather than in Google Calendar with this system reading from it is a separate
question.

The argument for owning them: the same conversational interface, and reminders and
events on one timeline. The argument against: calendars are a solved problem with
excellent clients, and reimplementing one to get a chat interface may be a poor
trade.

**Worth reassessing at P6,** once the `.ics` export shows what the combined view
actually looks like.

### Q-12: Apple Calendar write path

iCloud CalDAV requires an app-specific password and is reportedly fragile. An
alternative is writing to Google and letting Apple subscribe, which is one sync
integration instead of two.

---

## Needs an answer only if commercialization happens

### Q-13: Commercialization shape

Undecided, and quite possibly never decided. The product being built is the
single-user one in S1, and it is finished and worth having whether or not anything
follows it. This entry exists so that a later reader does not mistake "we built one
thing" for "we ruled out the others."

Two shapes are plausible and they want nearly opposite things:

| | Sell-and-self-host | Multi-tenant SaaS |
|---|---|---|
| Owner column on every table | never needed | required |
| Caller-identity resolution, identity mapping | never needed | required |
| Per-person config and secrets | never needed | required |
| Loop partitioning, incremental materialization | never needed | eventually |
| Nothing personal compiled in | **required** | **required** |
| Secrets generated per install | **required** | required |
| All SQL behind one module | useful | **required** |

Only the last three rows are shared, and all three were adopted because they are
worth having for a single user on their own merits — D9/D10, the config resolvers
in [06-agent-spec.md](06-agent-spec.md#system-prompt-structure), and the
repository module in [03-architecture.md](03-architecture.md#storage). Nothing in
this repository has been built *toward* multi-tenancy, and nothing should be.

If the question ever becomes live, the surfaces that move are: the repository
module, the singleton keys in [`kv`](04-data-model.md#kv), and config resolution.
That list is short because those three things are the only places the assumption
is expressed. Keeping it short is the entire hedge.

**Resolve by:** using the thing for six months first. The shape of the answer
depends on facts not yet in evidence, and the cost of leaving it open is now
approximately zero.

**One note if it does go multi-tenant:** cross-tenant isolation is the single
property in this system that cannot be verified by hand, because a leak looks
exactly like correct behaviour until it does not. That is not an argument against
S2 today — it is the specific condition under which S2 would need revisiting, and
what it would need is one test asserting that a query for one owner never returns
another owner's rows. One test, not a suite.

---

## Known unknowns

Things that will only become clear through use.

- **Whether notifications get ignored the way any notification does.** Buttons on
  the message are meant to lower resolution friction. If the notifications
  themselves get swiped away without reading, no button placement fixes that —
  not in the chat message and not on the lock screen — and the answer is fewer
  reminders rather than better ones. This sits underneath Q-15 and is the reason
  Q-15 can be answered "neither" rather than only "here or there".

- **Whether the personality is a feature or a novelty.** The honest expectation is
  that it charms for a fortnight. The tone ladder and the dormancy branch are the
  parts most likely to still be valuable at month six, because they change
  behaviour rather than phrasing.

- **Whether fuzzy schedules are actually pleasant.** A reminder at an unpredictable
  time is either a good way to break habituation or a low-grade irritation.
  Unknown until used.

- **What the real tier-one success rate is.** The escalation ladder is designed
  around an assumption that a small model handles most CRUD. `llm_calls` will say
  within two weeks, and the routing table should be rewritten from that rather
  than defended.

- **Whether `resolution_source` reveals something actionable.** If nearly
  everything gets resolved from the day view rather than notifications, the
  notification design is wrong. If nearly everything is resolved by messaging, the
  web app was not worth building. Q-15 now depends on this column, which promotes
  it from an interesting series to the one that settles a build decision.

---

## Deliberately not revisiting

Recorded so they do not get relitigated without new information.

- **Multi-user, sharing, delegation.** Out of scope for the product being built.
  Not the same claim as "impossible later" — see Q-13 for what the design does
  and does not foreclose.
- **A native iOS app.** APNs with notification categories is the only fully
  reliable path to native lock-screen actions, and it costs a developer account
  and a build pipeline. If those actions turn out to be worth having, T10 buys
  most of the benefit for an adapter and a token scheme, which is why Q-15 is the
  live question and this one is not.
- **A test suite.** Explicitly declined. `/healthz` and the metrics in D-023 cover
  the failure that actually matters, which is a stalled loop. Q-14 is the single
  narrow carve-out under discussion and is not an opening to revisit this.
- **Postgres.** Decided in D-002 and superseding an earlier decision. Only revisit
  if multi-replica ever becomes necessary. D-002 now records the one construct
  that would not port.
