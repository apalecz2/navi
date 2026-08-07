package schedule

import (
	"fmt"
	"time"
)

// Resolving a local wall clock to a UTC instant is the one piece of time
// arithmetic in this system that is written rather than inherited, because the
// stdlib resolves the two DST edge cases silently and not the way
// docs/05-schedule-spec.md#dst requires.
//
// It happens at materialization rather than at fire time. Occurrences exist
// only 30 days ahead and are re-materialized nightly, so every row that crosses
// a transition is generated after the rule is already known to the tz database.

// Fold reports which side of a daylight-saving transition a wall clock landed
// on. It is returned rather than logged here because this package does not log,
// and the materializer wants it at debug: a reminder that moved is worth being
// able to explain, and the two cases are rare enough that the line is readable
// when it appears.
type Fold uint8

const (
	// FoldNone is the ordinary case: the wall clock happened exactly once.
	FoldNone Fold = iota

	// FoldGap is a wall clock that never happened, inside the hour a
	// spring-forward transition skips.
	FoldGap

	// FoldAmbiguous is a wall clock that happened twice, inside the hour an
	// autumn transition repeats.
	FoldAmbiguous
)

// String names the fold for a log line.
func (f Fold) String() string {
	switch f {
	case FoldNone:
		return "none"
	case FoldGap:
		return "gap"
	case FoldAmbiguous:
		return "ambiguous"
	default:
		return fmt.Sprintf("fold(%d)", uint8(f))
	}
}

// Instant resolves a local wall clock to a UTC instant, deciding both DST edge
// cases deliberately:
//
//   - A nonexistent local time, 02:30 on a spring-forward day, becomes the first
//     valid time after the gap — 03:00, not 03:30.
//   - An ambiguous local time, 01:30 on a fall-back day, becomes the first of the
//     two, the pre-transition one.
//
// Neither is what time.Date does, and its own documentation says the choice is
// not guaranteed. Both directions are observable in the tzdb this binary
// embeds: for the gap it normalizes 02:30 in America/New_York backward, to
// 01:30 EST — an hour before what was asked for and on the wrong side of the
// transition — while in Australia/Lord_Howe it normalizes forward. For the fold
// it picks an offset, sometimes the earlier and sometimes the later, and reports
// neither that it chose nor that there was a choice.
//
// So neither case is inherited and neither assumes which way the stdlib went.
// The mechanism is time.ZoneBounds, which reports the extent of the zone period
// an instant falls in; that finds a transition without searching for one, and a
// transition is the whole of what both rules need.
func Instant(d LocalDateTime, loc *time.Location) (time.Time, Fold) {
	t := d.In(loc)

	// The wall clock did not survive the round trip, so it never existed. t is
	// whatever time.Date normalized it to, on one side of the gap or the other,
	// and the answer either way is the transition instant itself — which is the
	// boundary of t's zone period on the side t was pushed away from.
	if !d.equals(t) {
		start, end := t.ZoneBounds()
		if d.naive().Before(wall(t)) {
			if !start.IsZero() {
				return start.UTC(), FoldGap
			}
		} else if !end.IsZero() {
			return end.UTC(), FoldGap
		}
		return t.UTC(), FoldGap
	}

	// The wall clock exists. It is ambiguous when the clock goes back within
	// shift of t, and t can sit on either side of that transition depending on
	// which twin time.Date picked. Both are checked, and the answer is always
	// the earlier of the two.
	start, end := t.ZoneBounds()

	// t is after the transition: the earlier twin is shift before it.
	if !start.IsZero() {
		_, after := t.Zone()
		_, before := start.Add(-time.Second).Zone()

		// Derived rather than assumed to be an hour, because Lord Howe shifts by
		// thirty minutes and a hardcoded hour would miss its fold entirely.
		if shift := offsetShift(before, after); shift > 0 && t.Sub(start) < shift {
			if earlier := t.Add(-shift); d.equals(earlier) {
				return earlier.UTC(), FoldAmbiguous
			}
		}
	}

	// t is before the transition, so it is already the earlier twin. Nothing to
	// move; this branch exists to report the fold rather than to correct it.
	if !end.IsZero() {
		_, current := t.Zone()
		_, next := end.Zone()
		if shift := offsetShift(current, next); shift > 0 && end.Sub(t) <= shift {
			return t.UTC(), FoldAmbiguous
		}
	}

	return t.UTC(), FoldNone
}

// offsetShift is how far the clock goes back between two zone offsets, in
// seconds east of UTC. Zero or negative means it went forward, which is the gap
// and not this function's business.
func offsetShift(before, after int) time.Duration {
	return time.Duration(before-after) * time.Second
}

// equals reports whether an instant reads back as this wall clock in its own
// location. Comparing the fields rather than formatting avoids deciding on a
// layout for a comparison nobody sees.
func (d LocalDateTime) equals(t time.Time) bool {
	y, m, day := t.Date()
	h, min, sec := t.Clock()
	return y == d.Year && m == d.Month && day == d.Day &&
		h == d.Hour && min == d.Minute && sec == d.Second
}

// naive is the wall clock as an instant in UTC. It is not a real time and is
// never returned; it exists so two wall clocks can be ordered without a zone
// getting involved in the comparison.
func (d LocalDateTime) naive() time.Time {
	return time.Date(d.Year, d.Month, d.Day, d.Hour, d.Minute, d.Second, 0, time.UTC)
}

// wall is the reverse: an instant's local wall clock, stripped of its zone.
func wall(t time.Time) time.Time {
	y, m, d := t.Date()
	h, min, sec := t.Clock()
	return time.Date(y, m, d, h, min, sec, 0, time.UTC)
}
