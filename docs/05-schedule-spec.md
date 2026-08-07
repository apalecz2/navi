# Schedule Specification

This document defines what goes in `items.schedule`, how it becomes occurrence
rows, and what happens when it changes.

## Schedule kinds

A tagged union stored as JSON. Four kinds, discriminated by `kind`.

### `one_off`

```json
{
  "kind": "one_off",
  "at": "2026-08-14T10:00:00"
}
```

Local wall-clock time, naive, resolved against `items.tz`. Explicit rather than an
RRULE with `COUNT=1`, because the validation rules differ (must be in the future,
must be under two years out) and because one-offs never re-materialize.

### `fixed`

RRULE for which days, one fixed time of day.

```json
{
  "kind": "fixed",
  "rrule": "FREQ=DAILY",
  "at": "09:00"
}
```

```json
{
  "kind": "fixed",
  "rrule": "FREQ=WEEKLY;BYDAY=TU,TH",
  "at": "18:30"
}
```

### `windowed`

RRULE for which days, a random time within a window on each of those days.

```json
{
  "kind": "windowed",
  "rrule": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR",
  "window": ["09:00", "17:00"]
}
```

Covers "weekdays, once a day, randomly during the day". The randomness resolves at
materialization, so the calendar shows a real time, not a range.

### `fuzzy`

N times per period, distributed across allowed days with a minimum gap.

```json
{
  "kind": "fuzzy",
  "period": "week",
  "count": 3,
  "days_allowed": ["MO", "TU", "WE", "TH", "FR"],
  "window": ["09:00", "21:00"],
  "min_gap_hours": 20
}
```

`period` is `day`, `week`, or `month`. This is the kind that handles "remind me
periodically through the week", which has no RRULE representation because RRULE
expresses *which* days, not *how many times, somewhere in here*.

`min_gap_hours` is what makes it usable. Without it, "three times this week" is a
legitimate draw of Monday 09:05, Monday 09:40, and Monday 15:00, which is not what
anyone means.

## Why RRULE

Three reasons, in order of weight.

1. It is what Google Calendar and CalDAV consume. Choosing anything else means
   writing a translation layer the day calendar sync starts.
2. Models emit it reliably, because the syntax is heavily represented in training
   data. A bespoke DSL would need examples and would still be guessed at.
3. `teambition/rrule-go` parses and expands it, so no date arithmetic gets
   written by hand.

The cost is that `fuzzy` sits outside it. That is accepted: fuzzy schedules are
reminder-only and export to calendars as individual events rather than as a
recurrence rule.

A second cost is newer. `rrule-go` is a good library and it is not
`python-dateutil`, which has two decades of edge cases beaten out of it. The
places that matter are expansion across a DST boundary and `BYDAY` with a
negative index. Both are covered by [Q-14](10-open-questions.md#q-14-rrule-and-dst-expansion-correctness),
which is the one place where S2 is under discussion.

## Vocabulary defaults

Loaded from `/config/defaults.yaml`. The model resolves phrasing through this
table rather than improvising, so the same words produce the same schedule every
time and retuning is a config edit.

```yaml
frequency:
  "periodically":       { kind: fuzzy, period: week, count: 3 }
  "now and then":       { kind: fuzzy, period: week, count: 2 }
  "every so often":     { kind: fuzzy, period: week, count: 2 }
  "a couple of times":  { count: 2 }
  "a few times":        { count: 3 }
  "regularly":          { kind: fixed, rrule: "FREQ=DAILY" }
  "often":              { kind: fuzzy, period: week, count: 5 }

windows:
  "morning":            ["07:00", "11:00"]
  "afternoon":          ["12:00", "17:00"]
  "evening":            ["17:00", "22:00"]
  "during the day":     ["09:00", "18:00"]
  default:              ["09:00", "21:00"]

min_gap_hours:
  day:                  3
  week:                 20
  month:                48

defaults:
  days_allowed:         ["MO","TU","WE","TH","FR","SA","SU"]
  priority:             3
  notify_policy:        at_time
  tz_mode:              floating
  snooze_cap:           3
```

## Materialization

Runs nightly, and synchronously on any schedule change so the confirmation can
show real timestamps.

**Horizon:** 30 days.

Each item is read, planned, and written inside **one transaction**, because the
plan is decided by looking at what already exists and a read taken before the
write opened can be stale by the time it lands. The nightly run and the
synchronous re-materialization on a schedule change are two callers of exactly
that.

```
for each active, non-archived item:
    begin
    existing = occurrences where item_id = item.id and starts_at > now
    eligible = existing minus anything inside a pause window
    wanted   = expand(item.schedule, from=now, to=now+30d, tz=resolve_tz(item),
                      already=eligible)
    for each slot in wanted:
        if slot is already filled by an eligible row: keep that row
        else: insert occurrence(status='pending')
    delete every pending, non-override, future row that filled no slot
    commit
update kv.last_materialized_through   # only if no item failed
```

Keep-and-top-up rather than delete-and-regenerate. The resulting set is the same
and two things fall out of it that the other ordering loses: a drawn time that
already exists is kept rather than redrawn, which is what makes the idempotence
below true for `windowed` and `fuzzy` at all (D-005); and a row the copywriter has
already generated `message_text` for survives the night instead of being deleted
and rewritten empty for another model call to fill.

A **paused** item is not skipped, it is materialized to an empty set. Skipping it
would leave the pending rows already sitting inside a newly-created pause window,
which is the opposite of what [Pause](#pause) asks for; an empty wanted set
deletes exactly those and leaves history and overrides alone. An **archived or
inactive** item takes the same path, which is what makes archiving clear the
calendar without a second code path.

Three invariants:

- **Only `pending`, non-override, future rows are touched.** Everything else is
  history or a deliberate exception. This is enforced in the `WHERE` clause of
  the delete rather than by the planner, so a planner bug cannot reach history.
- **Overrides survive.** A row with `is_override = 1` is never deleted or
  overwritten, which is the entire mechanism behind "skip tomorrow's".
- **Idempotent.** Running it twice produces the same set. New random draws happen
  only for slots that did not already exist, so re-running does not reshuffle
  times you have already seen on the calendar.

What counts as a slot differs by kind, and that predicate is the whole of the
third invariant:

| Kind | A slot is | Filled when |
|---|---|---|
| `one_off`, `fixed` | the exact instant | a future row has that `starts_at` |
| `windowed` | the local **date** | a future row falls on that date, at a time the current window still allows |
| `fuzzy` | the local **period** | counted, not matched: a week needing three that holds two draws one more |

The two drawn kinds match coarser than the instant deliberately. Matching on the
instant would make every run a fresh draw, because a redrawn time never equals the
one it replaced.

### Expansion by kind

**`one_off`:** one occurrence, if `at` is in the future.

**`fixed`:** expand the RRULE across the range, combine each date with `at` in the
item's timezone, convert to UTC.

The rule is expanded **in UTC and for dates only**, from a `DTSTART` built out of
the item's creation date, and never sees the item's timezone. RRULE expresses
which days rather than when on them, so a zone has nothing to contribute to that
half; and keeping `rrule-go`'s arithmetic away from DST leaves exactly one
conversion in the system that can get a transition wrong, which is the one
[written for it](#dst). Anchoring on `created_at` rather than on today is what
makes a rule with no `BYDAY` stable — `FREQ=WEEKLY` means "every seven days from
`DTSTART`", so an anchor of today would walk the item to a new weekday on every
nightly run.

**`windowed`:** expand the RRULE, and for each date draw a uniform random time
inside the window on a 5 minute grid, because 14:37 reads as machine-generated
noise and 14:35 does not.

The draw is from the grid rather than a free minute rounded onto it. Rounding
pushes a time out of its own window at both ends — 20:58 in a window closing at
21:00 rounds to 21:00, 09:01 in one opening at 09:00 rounds to 09:00 — and gives
the two end marks half the share of every other mark. Drawing from the marks
directly is uniform and cannot leave the window.

**`fuzzy`:** for each period in range:

```
slots = []
attempts = 0
while len(slots) < count and attempts < 200:
    attempts += 1
    day  = random choice from days_allowed within this period
    time = uniform random inside window, on the 5 min grid
    cand = combine(day, time) in item tz
    if cand < now: continue
    if any(abs(cand - s) < min_gap_hours for s in slots): continue
    slots.append(cand)
if len(slots) < count:
    relax min_gap by 25% and retry once, then accept what was found
```

The relaxation matters for tight cases, such as four times a week across three
allowed days with a 20 hour gap, which is nearly unsatisfiable. Better to place
three and move on than to fail the whole item.

### Partial periods

A `fuzzy` weekly item created on a Thursday should not try to fit three
occurrences into the remaining two days. Scale the count to the remaining fraction
of the period, **rounding down, minimum one**. So Thursday creation of a
3-per-week item produces one occurrence for the current partial week and three for
each full week after.

Rounding down rather than up, because rounding up does not produce that. Weeks
start on Monday, so a Thursday has four of seven days left and `ceil(3 × 4/7)` is
two; the fraction has to fall under a third before rounding up reaches one at all,
which no day of the week does. Rounding down, with the minimum carrying the last
day or two of a period:

| Created | Fraction left | Target |
|---|---|---|
| Monday | 7/7 | 3 |
| Wednesday | 5/7 | 2 |
| Thursday | 4/7 | 1 |
| Friday | 3/7 | 1 |
| Sunday | 1/7 | 1, by the minimum |

The fraction is measured in elapsed time rather than in whole days, so it means
the same thing for all three periods. A 3-per-day item created at 18:00 gets one
occurrence for what is left of the day, not three.

## Timezones

Every item carries an IANA timezone and a mode.

**`tz_mode = 'fixed'`.** Fire at that wall clock in that zone regardless of where
the device is. Correct for a standing call with someone in another country.

**`tz_mode = 'floating'`.** Fire at that wall clock wherever the device currently
is, read from `kv.current_tz`. Correct for stretching, meals, medication, and
almost everything else. This is the default.

`kv.current_tz` is updated by messaging the agent ("I'm in Lisbon this week").
Changing it triggers re-materialization of future pending occurrences for all
floating items.

### DST

Resolve local wall clock to UTC at materialization, not at fire time. Because
occurrences are materialized only 30 days ahead and re-materialized nightly, a
DST boundary is crossed by rows generated after the transition rule is already
known to the tz database.

Two edge cases in the spring-forward gap and the autumn overlap:

- Nonexistent local time (02:30 on a spring-forward day): shift forward to the
  first valid time, 03:00.
- Ambiguous local time (01:30 on a fall-back day): take the first occurrence, the
  pre-transition one.

Go's `time.Date` handles neither of these the way this spec requires, and it does
not report that it has made a choice. Its own documentation says the choice is not
guaranteed, and measurement against the embedded tzdb bears that out: 02:30 on
8 March 2026 in `America/New_York` comes back as **01:30 EST** — an hour *before*
what was asked for, on the far side of the transition — while the same gap in
`Australia/Lord_Howe` normalizes the other way. For an ambiguous time it picks an
offset, sometimes the earlier and sometimes the later, and reports neither that it
chose nor that there was a choice.

So both cases are resolved explicitly rather than inherited, and neither assumes
which way the stdlib went. Construct the time and check whether it reads back as
the requested wall clock. If it does not, the time never existed, and the answer
is the transition instant itself — reachable from `time.ZoneBounds`, which reports
the extent of the zone period an instant falls in, without searching for a
transition. If it does read back, the same bounds say whether a clock going
backwards puts a second instant on the same wall clock, in which case the earlier
one wins. The size of the shift is derived from the two offsets rather than
assumed to be an hour, because Lord Howe moves by thirty minutes.

This is roughly twenty lines and it is worth writing them, because the failure
mode is a reminder firing an hour off twice a year with no explanation, which is
exactly the kind of bug that gets misfiled as flakiness.

It lives in `internal/schedule/dst.go` as `Instant`, beside the naive
`LocalDateTime.In` that validation uses, and it returns which of the two cases it
hit so the materializer can say so at debug.

`time/tzdata` is imported for the embedded database (see D11 — a `scratch` image
has no system zoneinfo).

## Edit scope

`update_item` takes an explicit `scope` rather than inferring intent.

| Scope | Behaviour | Example phrasing |
|---|---|---|
| `future_all` (default) | Delete all future `pending` non-override occurrences, re-materialize | "move my vitamins to 8pm" |
| `from_date` | Same, bounded to start at a given date | "starting in September, three times a week" |
| `single` | Modify or cancel exactly one occurrence, set `is_override = 1` | "skip tomorrow's", "push Thursday's to Friday" |

Rules:

- **History is immutable.** Only `pending` rows are ever deleted.
- **A pending occurrence later today is regenerated by default,** because "move my
  meds to 8pm" should affect tonight. If the new time has already passed today,
  the change takes effect tomorrow.
- **Field-level diff before re-materializing.** If only `title`, `notes`,
  `priority`, or `attrs` changed, skip materialization entirely. A typo fix should
  not churn the calendar.

## Snooze

Snooze creates a child occurrence. It does not mutate the original.

```
snooze(occurrence, delta):
    if occurrence.snooze_depth >= item.snooze_cap:
        resolve chain as 'missed'
        return

    occurrence.status = 'snoozed'
    occurrence.resolved_at = now
    # starts_at is NOT modified

    insert occurrence(
        item_id              = occurrence.item_id,
        starts_at            = resolve_delta(delta, item),
        status               = 'pending',
        is_override          = 1,
        parent_occurrence_id = occurrence.id,
        snooze_depth         = occurrence.snooze_depth + 1,
    )
```

`is_override = 1` on the child stops the next materialization run from deleting
it.

### Delta resolution

| Preset | Resolution |
|---|---|
| `10m`, `1h` | Simple offset from now |
| `tonight` | 19:00 in the item's effective timezone, or now + 2h if already past |
| `tomorrow` | The item's normal time on the next day, or 09:00 if it has no fixed time |

Relative terms resolve against the item's timezone and window, not by naive
arithmetic. "Tomorrow" on a 07:00 reminder means 07:00 tomorrow, not this time
tomorrow.

### Why a child row

Mutating `starts_at` in place would be simpler and is wrong. It destroys the
record that the occurrence was originally due at 09:00 and got pushed three times,
which is exactly the signal the copywriter and the proposal logic need. It also
silently rewrites the calendar, so looking back at last Tuesday would show a time
that never fired.

### Chains and statistics

See [04-data-model.md](04-data-model.md#streaks-and-snooze-chains). A chain counts
once. If any link completes, the chain completed and the streak survives.

The reasoning matters enough to restate: if snoozing broke streaks, the rational
response would be to ignore the notification rather than snooze it, which produces
no data at all. Making honest snoozing free is what keeps the dataset truthful.

### Repeated snoozes as a signal

Three snoozes on one occurrence means the scheduled time is wrong, not that
discipline failed. It triggers the same proposal path as repeated misses: the
agent suggests a different time, and does not apply it unilaterally.

## Pause

Two levels, both honoured by the materializer, the scheduler, and the reconciler.

- **Per item:** `items.paused_until`
- **Global:** `kv.global_pause_until`

Occurrences inside a pause window are not generated. Occurrences that already
exist inside a newly-created pause window are deleted if `pending` and left alone
otherwise.

Pausing exists because "I'm away until Monday" should not resolve to eighteen
individual skips. It also produces cleaner statistics than a wall of skips would.

## Validation

Every schedule is validated before any write, by code, not by the model. Failures
feed the escalation ladder in [06-agent-spec.md](06-agent-spec.md).

| Check | Rule |
|---|---|
| RRULE parses | `rrule.StrToRRule` succeeds |
| Produces occurrences | Expands to at least one occurrence within 90 days |
| Not absurdly dense | Under 50 occurrences in 30 days |
| Timezone valid | `time.LoadLocation` succeeds |
| Window ordered | `window[0] < window[1]` |
| Window wide enough | At least 30 minutes |
| Count sane | `1 <= count <= 20` per period |
| Gap satisfiable | `count * min_gap_hours <= period_hours * 1.5` |
| One-off in future | `at > now` |
| One-off bounded | `at < now + 2 years` |
| Item exists | Referenced `item_id` resolves to a non-archived row |

The gap check is the one that catches genuine nonsense such as "five times a day,
at least eight hours apart", which is arithmetically impossible and would
otherwise burn 200 placement attempts before quietly producing three.
