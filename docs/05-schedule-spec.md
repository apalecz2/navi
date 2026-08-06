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

```
for each active, non-archived item:
    if item.paused_until > horizon_start: skip
    existing = occurrences where item_id = item.id
                 and starts_at > now
                 and status = 'pending'
                 and is_override = 0
    delete existing
    generate = expand(item.schedule, from=now, to=now+30d, tz=resolve_tz(item))
    for each candidate in generate:
        if candidate falls inside a pause window: skip
        if an override already exists at that slot: skip
        insert occurrence(status='pending')
update kv.last_materialized_through
```

Three invariants:

- **Only `pending`, non-override, future rows are touched.** Everything else is
  history or a deliberate exception.
- **Overrides survive.** A row with `is_override = 1` is never deleted or
  overwritten, which is the entire mechanism behind "skip tomorrow's".
- **Idempotent.** Running it twice produces the same set. New random draws happen
  only for slots that did not already exist, so re-running does not reshuffle
  times you have already seen on the calendar.

### Expansion by kind

**`one_off`:** one occurrence, if `at` is in the future.

**`fixed`:** expand the RRULE across the range, combine each date with `at` in the
item's timezone, convert to UTC.

**`windowed`:** expand the RRULE, and for each date draw a uniform random minute
inside the window. Round to the nearest 5 minutes, because 14:37 reads as
machine-generated noise and 14:35 does not.

**`fuzzy`:** for each period in range:

```
slots = []
attempts = 0
while len(slots) < count and attempts < 200:
    attempts += 1
    day  = random choice from days_allowed within this period
    time = uniform random inside window, rounded to 5 min
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
of the period, rounding up, minimum one. So Thursday creation of a 3-per-week item
produces one occurrence for the current partial week and three for each full week
after.

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
not report that it has made a choice. For a nonexistent time it normalizes
forward, which happens to match rule one by accident. For an ambiguous time it
picks one offset with no indication that two were available, which does not
reliably match rule two.

So both cases are resolved explicitly rather than inherited: construct the time,
then check whether `t.Format` round-trips to the requested wall clock. If it does
not, the time did not exist and the normalized result is correct. For the
ambiguous case, probe one hour either side and take the earlier offset
deliberately. This is roughly twenty lines and it is worth writing them, because
the failure mode is a reminder firing an hour off twice a year with no
explanation, which is exactly the kind of bug that gets misfiled as flakiness.

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
