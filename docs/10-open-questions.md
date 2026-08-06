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

## Known unknowns

Things that will only become clear through use.

- **Whether ntfy notifications get ignored the way any notification does.** The
  action buttons are meant to lower resolution friction to near zero. If the
  notifications themselves get swiped away without reading, no button placement
  fixes that, and the answer is fewer reminders rather than better ones.

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
  web app was not worth building.

---

## Deliberately not revisiting

Recorded so they do not get relitigated without new information.

- **Multi-user, sharing, delegation.** Out of scope, and the schema choices assume
  it stays that way.
- **A native iOS app.** APNs with notification categories is the only fully
  reliable path to native actions, and it costs a developer account and a build
  pipeline. ntfy gets close enough.
- **A test suite.** Explicitly declined. `/healthz` covers the failure that
  actually matters, which is a stalled loop.
- **Postgres.** Decided in D-002 and superseding an earlier decision. Only revisit
  if multi-replica ever becomes necessary, which would require multi-user, which
  is out of scope.
