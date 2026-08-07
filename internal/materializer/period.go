package materializer

import (
	"time"

	"github.com/aidenpaleczny/navi/internal/schedule"
)

// bucket is one period of a fuzzy schedule inside the run's window: the days it
// may place occurrences on, and how much of it is actually available.
//
// The boundaries are instants rather than dates because full and usable are
// durations, and a week containing a DST transition is 167 or 169 hours rather
// than 168. Getting that wrong would shift a partial period's target by a
// fraction of an occurrence twice a year, which is exactly the class of bug this
// component is built to not have.
type bucket struct {
	start time.Time
	end   time.Time

	first localDate
	last  localDate

	loc *time.Location

	// full is the length of the whole period, usable the part of it that is both
	// ahead of now and inside the horizon.
	full   time.Duration
	usable time.Duration
}

// holds reports whether an instant falls in this period.
func (b bucket) holds(at time.Time) bool {
	return !at.Before(b.start) && at.Before(b.end)
}

// days lists the dates in this period that the schedule is allowed to use and
// that the run's window actually reaches. Restricting to the window here rather
// than rejecting inside the placement loop matters: a draw that lands on a day
// outside the horizon costs one of two hundred attempts, and a period that is
// mostly outside the horizon would spend them all.
func (b bucket) days(allowed map[time.Weekday]bool) []localDate {
	var out []localDate
	for d := b.first.utc(); !d.After(b.last.utc()); d = d.AddDate(0, 0, 1) {
		if allowed[d.Weekday()] {
			out = append(out, dateOf(d))
		}
	}
	return out
}

// buckets divides the run's window into periods of the given length.
//
// Periods are calendar-aligned rather than anchored on the item: a weekly item
// means "three times a week", and which week that is has to be the same week the
// user means, not one starting on whichever day the item happened to be created.
// Weeks start on Monday, which is RRULE's default and the schedule vocabulary's
// day order.
func (rq request) buckets(p schedule.Period) []bucket {
	from := rq.run.now.In(rq.loc)
	to := rq.run.to.In(rq.loc)

	var out []bucket
	for d := startOf(p, dateOf(from)); !d.utc().After(dateOf(to).utc()); d = next(p, d) {
		b := bucket{first: d, last: dateOf(next(p, d).utc().AddDate(0, 0, -1)), loc: rq.loc}

		b.start = rq.midnight(d)
		b.end = rq.midnight(next(p, d))
		b.full = b.end.Sub(b.start)

		// A period straddling either edge of the run's window contributes only
		// the part inside it, which is what scaleCount reads.
		start, end := b.start, b.end
		if start.Before(rq.run.now) {
			start = rq.run.now
		}
		if end.After(rq.run.to) {
			end = rq.run.to
		}
		b.usable = end.Sub(start)

		// Clamp the day list too, so placement never draws a day the run cannot
		// use at all.
		if b.first.utc().Before(dateOf(from).utc()) {
			b.first = dateOf(from)
		}
		if b.last.utc().After(dateOf(to).utc()) {
			b.last = dateOf(to)
		}

		out = append(out, b)
	}
	return out
}

// midnight is the start of a local day as an instant, through the same
// conversion every other wall clock goes through. Some zones transition at
// midnight — Santiago does — so this is not a case where time.Date would do.
func (rq request) midnight(d localDate) time.Time {
	at, _ := schedule.Instant(schedule.LocalDateTime{Year: d.year, Month: d.month, Day: d.day}, rq.loc)
	return at
}

// startOf is the first day of the period containing d.
func startOf(p schedule.Period, d localDate) localDate {
	switch p {
	case schedule.PeriodWeek:
		// Monday is weekday 0 for this purpose; Go's Sunday is 0.
		back := (int(d.utc().Weekday()) + 6) % 7
		return dateOf(d.utc().AddDate(0, 0, -back))
	case schedule.PeriodMonth:
		return localDate{year: d.year, month: d.month, day: 1}
	default:
		return d
	}
}

// next is the first day of the period after the one starting at d.
func next(p schedule.Period, d localDate) localDate {
	switch p {
	case schedule.PeriodWeek:
		return dateOf(d.utc().AddDate(0, 0, 7))
	case schedule.PeriodMonth:
		return dateOf(d.utc().AddDate(0, 1, 0))
	default:
		return dateOf(d.utc().AddDate(0, 0, 1))
	}
}
