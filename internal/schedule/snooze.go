package schedule

import (
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Snooze deltas are the four presets in docs/05-schedule-spec.md#delta-resolution
// (R9). They resolve here rather than beside the endpoint for the reason this
// package's doc comment already gives: two of the four are wall-clock questions
// about a zone, which is what Instant answers, and the answer must be the same
// one materialization gets.
//
// Nothing here touches the database or the clock beyond the now it is handed. A
// caller inside an open write transaction can run it safely, which is what
// store.SnoozeOccurrence's callback does.

// Snooze offsets for the two presets that are pure instant arithmetic.
const (
	Snooze10m = 10 * time.Minute
	Snooze1h  = time.Hour
)

// TonightHour is what "tonight" means as a local wall clock.
const TonightHour = 19

// TonightFallback is how far out "tonight" lands when 19:00 has already gone.
// An hour is too close to be a snooze and three is tomorrow; two is what the
// spec's table says.
const TonightFallback = 2 * time.Hour

// FallbackTimeOfDay is "tomorrow"'s last resort, from the spec's "or 09:00 if it
// has no fixed time".
//
// It is genuinely a last resort and not the windowed/fuzzy answer. Those kinds
// resolve to their window start (see NormalTimeOfDay), which is 09:00 anyway for
// the default window in defaults.yaml — so the spec's literal answer falls out
// for the ordinary case, and an item whose window is 17:00-22:00 snoozes into an
// evening it actually fires in rather than into a morning it never does. This
// constant is reached only when the stored schedule names no time at all: an
// unparseable at, or a windowed/fuzzy column with no window. Resolve fills the
// window on every write path, so a row written through this codebase cannot get
// here.
var FallbackTimeOfDay = LocalTime{Hour: 9}

// Delta is a snooze preset. The set is closed: R9 says presets, and a free-form
// duration is a schedule change asking to be mistaken for a snooze.
type Delta string

const (
	Delta10m      Delta = "10m"
	Delta1h       Delta = "1h"
	DeltaTonight  Delta = "tonight"
	DeltaTomorrow Delta = "tomorrow"
)

// Deltas is the set, in the order the spec's table lists them. It is what the
// "not one of" message enumerates, so the message and the switch cannot
// disagree — the same arrangement Kinds and Periods have.
var Deltas = []Delta{Delta10m, Delta1h, DeltaTonight, DeltaTomorrow}

// Valid reports whether a delta is one of the four.
func (d Delta) Valid() bool {
	switch d {
	case Delta10m, Delta1h, DeltaTonight, DeltaTomorrow:
		return true
	}
	return false
}

// deltaList renders the four for an error message.
func deltaList() string {
	names := make([]string, len(Deltas))
	for i, d := range Deltas {
		names[i] = string(d)
	}
	return strings.Join(names, ", ")
}

// NormalTimeOfDay is the wall clock an item ordinarily fires at, which is what
// "tomorrow" means by "the item's normal time".
//
//	one_off   the HH:MM inside at
//	fixed     at
//	windowed  the window's start
//	fuzzy     the window's start
//
// The two drawn kinds have no single time — that is the whole of what drawn
// means — so the window's opening is the nearest honest answer, and R9 asks for
// exactly that: relative terms resolve against the item's timezone *and window*.
//
// ok is false when the schedule names no time this function can read, in which
// case the caller uses FallbackTimeOfDay. It is a bool rather than an error
// because "this schedule has no time of day" is an answer, not a failure.
func NormalTimeOfDay(s Schedule) (LocalTime, bool) {
	switch s.Kind {
	case KindOneOff:
		d, err := s.OneOffAt()
		if err != nil {
			return LocalTime{}, false
		}
		return LocalTime{Hour: d.Hour, Minute: d.Minute}, true

	case KindFixed:
		t, err := s.TimeOfDay()
		if err != nil {
			return LocalTime{}, false
		}
		return t, true

	case KindWindowed, KindFuzzy:
		w, err := s.WindowTimes()
		if err != nil {
			return LocalTime{}, false
		}
		return w.Start, true
	}
	return LocalTime{}, false
}

// ResolveDelta turns a preset into the instant a snooze child starts at.
//
// The two arithmetic presets and the two wall-clock ones are deliberately not
// the same operation, and conflating them is the bug this function exists to
// avoid:
//
//   - 10m and 1h are offsets on the instant. Ten minutes from now is ten real
//     minutes even across a transition, so 01:55 + 1h on a spring-forward night
//     is 03:55 local and that is correct.
//   - tonight and tomorrow are wall clocks in the item's zone and go through
//     Instant, never through Add(24 * time.Hour). "09:00 tomorrow" across a
//     spring-forward boundary is 23 hours away, and naive arithmetic would put
//     the reminder at 10:00.
//
// The returned Fold reports which DST edge case Instant hit, so a caller can log
// it at debug the way the materializer does. It is always FoldNone for the two
// offsets, which never resolve a wall clock at all.
func ResolveDelta(d Delta, s Schedule, loc *time.Location, now time.Time) (time.Time, Fold, error) {
	if loc == nil {
		return time.Time{}, FoldNone, domain.Invalid("timezone_valid", "tz",
			"a snooze delta needs a timezone to resolve against")
	}

	switch d {
	case Delta10m:
		return now.Add(Snooze10m).UTC(), FoldNone, nil

	case Delta1h:
		return now.Add(Snooze1h).UTC(), FoldNone, nil

	case DeltaTonight:
		local := now.In(loc)
		at, fold := Instant(
			Combine(local.Year(), local.Month(), local.Day(), LocalTime{Hour: TonightHour}),
			loc,
		)
		// Already past, so tonight has nothing left to offer and the spec falls
		// back to a plain offset rather than to tomorrow evening — a snooze the
		// user will not see for a day is not what "tonight" asked for.
		if !at.After(now) {
			return now.Add(TonightFallback).UTC(), FoldNone, nil
		}
		return at, fold, nil

	case DeltaTomorrow:
		tod, ok := NormalTimeOfDay(s)
		if !ok {
			tod = FallbackTimeOfDay
		}
		local := now.In(loc)

		// Date-only arithmetic, in UTC, so the day increments by a day rather
		// than by 24 hours. The wall clock is attached afterwards and resolved
		// by Instant, which is what makes the spring-forward case come out 23
		// hours later instead of shifting the reminder by an hour.
		next := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).
			AddDate(0, 0, 1)

		at, fold := Instant(Combine(next.Year(), next.Month(), next.Day(), tod), loc)
		return at, fold, nil
	}

	return time.Time{}, FoldNone, domain.Invalid("snooze_delta_valid", "delta",
		"delta %q is not one of %s", string(d), deltaList())
}
