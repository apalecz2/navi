package materializer

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/teambition/rrule-go"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
)

// maxAttempts caps the fuzzy placement loop, from
// docs/05-schedule-spec.md#expansion-by-kind. A schedule that cannot be
// satisfied — five times a day at least eight hours apart — must not spin, and
// the validator's gap-satisfiable rule is what stops most of them arriving here
// at all.
const maxAttempts = 200

// relaxNumerator and relaxDenominator are the 25% the minimum gap is relaxed by
// when the first pass falls short. Four times a week across three allowed days
// at twenty hours apart is nearly unsatisfiable, and placing three is better
// than failing the item.
const (
	relaxNumerator   = 3
	relaxDenominator = 4
)

// slotStep is the granularity a drawn time lands on, in minutes (Q-3). 14:37
// reads as machine-generated noise and 14:35 does not.
const slotStep = 5

// slot is one instant the schedule wants filled, and the existing row that
// already fills it if there is one. A slot with no row becomes an insert; a slot
// with one is why re-materialization is idempotent.
type slot struct {
	at   time.Time
	have *domain.Occurrence
}

// localDate is a calendar day with no time and no zone. RRULE expansion answers
// in these and nothing else — see dates.
type localDate struct {
	year  int
	month time.Month
	day   int
}

func dateOf(t time.Time) localDate {
	y, m, d := t.Date()
	return localDate{year: y, month: m, day: d}
}

// utc renders the date as midnight UTC. It is not an instant anyone fires at; it
// is how dates are compared and iterated without a zone joining in.
func (d localDate) utc() time.Time {
	return time.Date(d.year, d.month, d.day, 0, 0, 0, 0, time.UTC)
}

func (d localDate) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.year, int(d.month), d.day)
}

// expand produces the slots the schedule wants inside the run's window, given
// the rows that already exist. Existing rows are passed in rather than consulted
// afterwards because two of the four kinds need them to decide what to generate:
// a drawn time that already exists must be kept, not drawn again (D-005).
func (rq request) expand(eligible []*domain.Occurrence) ([]slot, error) {
	var (
		slots []slot
		err   error
	)

	switch rq.schedule.Kind {
	case schedule.KindOneOff:
		slots, err = rq.expandOneOff(eligible)
	case schedule.KindFixed:
		slots, err = rq.expandFixed(eligible)
	case schedule.KindWindowed:
		slots, err = rq.expandWindowed(eligible)
	case schedule.KindFuzzy:
		slots, err = rq.expandFuzzy(eligible)
	default:
		return nil, domain.Invalid("kind_valid", "schedule.kind",
			"schedule kind %q is not one of %s", rq.schedule.Kind, kindList())
	}
	if err != nil {
		return nil, err
	}

	sort.Slice(slots, func(i, j int) bool { return slots[i].at.Before(slots[j].at) })
	return slots, nil
}

// expandOneOff is one occurrence, if it is still ahead. A one-off never
// re-materializes into anything new: once its instant is behind the run's
// window, the row that already fired is the whole of its history.
func (rq request) expandOneOff(eligible []*domain.Occurrence) ([]slot, error) {
	d, err := rq.schedule.OneOffAt()
	if err != nil {
		return nil, err
	}

	at := rq.instant(d)
	if !rq.wanted(at) {
		return nil, nil
	}
	return []slot{{at: at, have: firstAt(eligible, at)}}, nil
}

// expandFixed combines each date the rule names with the schedule's time of day.
func (rq request) expandFixed(eligible []*domain.Occurrence) ([]slot, error) {
	tod, err := rq.schedule.TimeOfDay()
	if err != nil {
		return nil, err
	}
	dates, err := rq.dates()
	if err != nil {
		return nil, err
	}

	slots := make([]slot, 0, len(dates))
	for _, d := range dates {
		at := rq.instant(schedule.Combine(d.year, d.month, d.day, tod))
		if !rq.wanted(at) {
			continue
		}
		slots = append(slots, slot{at: at, have: firstAt(eligible, at)})
	}
	return slots, nil
}

// expandWindowed draws a time inside the window for each date the rule names.
//
// The slot here is the date, not the instant: a date that already carries a row
// keeps the time that row was given. Matching on the instant instead would make
// every run a fresh draw, since a redrawn time never equals the old one, and the
// calendar would reshuffle nightly — which is exactly what D-005 accepts the
// materializer's skip-what-exists rule in order to avoid.
func (rq request) expandWindowed(eligible []*domain.Occurrence) ([]slot, error) {
	w, err := rq.schedule.WindowTimes()
	if err != nil {
		return nil, err
	}
	dates, err := rq.dates()
	if err != nil {
		return nil, err
	}

	slots := make([]slot, 0, len(dates))
	for _, d := range dates {
		if have := firstOnDate(eligible, d, rq.loc, w); have != nil {
			slots = append(slots, slot{at: have.StartsAt, have: have})
			continue
		}

		at := rq.instant(schedule.Combine(d.year, d.month, d.day, rq.draw(w)))
		if !rq.wanted(at) {
			continue
		}
		slots = append(slots, slot{at: at})
	}
	return slots, nil
}

// expandFuzzy places count occurrences per period, at least min_gap_hours apart.
//
// The slot here is the period, counted rather than matched: a week that already
// holds two of its three draws the third and leaves the two alone. That is the
// only predicate under which "run it twice and nothing changes" and "the times
// are random" are both true.
func (rq request) expandFuzzy(eligible []*domain.Occurrence) ([]slot, error) {
	s := rq.schedule
	if s.Period == nil || s.Count == nil || s.MinGapHours == nil || len(s.DaysAllowed) == 0 {
		return nil, domain.Invalid("field_required", "schedule",
			"a fuzzy schedule requires period, count, days_allowed and min_gap_hours")
	}
	w, err := rq.schedule.WindowTimes()
	if err != nil {
		return nil, err
	}
	allowed, err := weekdaySet(s.DaysAllowed)
	if err != nil {
		return nil, err
	}
	gap := time.Duration(*s.MinGapHours) * time.Hour

	var slots []slot
	for _, b := range rq.buckets(*s.Period) {
		target := scaleCount(*s.Count, b.usable, b.full)
		if target == 0 {
			continue
		}

		// Rows already in this period count towards the target, but only if the
		// current schedule would still have produced them. A row left over from
		// a window or a day-set that has since been edited is not a slot this
		// schedule wants, so it frees its place rather than blocking it.
		taken := make([]time.Time, 0, target)
		for _, occ := range eligible {
			if len(taken) >= target {
				break
			}
			if !b.holds(occ.StartsAt) || !fits(occ.StartsAt, rq.loc, allowed, w) {
				continue
			}
			slots = append(slots, slot{at: occ.StartsAt, have: occ})
			taken = append(taken, occ.StartsAt)
		}

		days := b.days(allowed)
		if len(days) == 0 {
			continue
		}
		for _, at := range rq.place(target-len(taken), days, w, gap, taken) {
			slots = append(slots, slot{at: at})
		}
	}
	return slots, nil
}

// place is the placement loop from docs/05-schedule-spec.md, verbatim in
// behaviour: draw a day and a time, reject anything in the past or inside the
// minimum gap, give up after maxAttempts, then relax the gap by a quarter and
// try once more before accepting whatever was found.
//
// taken seeds the gap check with the instants already placed in this period, so
// topping a week up from two occurrences to three respects the two that are
// already on the calendar.
func (rq request) place(need int, days []localDate, w schedule.Window, gap time.Duration, taken []time.Time) []time.Time {
	if need <= 0 {
		return nil
	}

	out := make([]time.Time, 0, need)
	attempt := func(gap time.Duration) {
		for attempts := 0; len(out) < need && attempts < maxAttempts; attempts++ {
			d := days[rq.rnd.IntN(len(days))]
			at := rq.instant(schedule.Combine(d.year, d.month, d.day, rq.draw(w)))
			if !rq.wanted(at) {
				continue
			}
			if tooClose(at, taken, gap) || tooClose(at, out, gap) {
				continue
			}
			out = append(out, at)
		}
	}

	attempt(gap)
	if len(out) < need {
		attempt(gap * relaxNumerator / relaxDenominator)
	}
	return out
}

// tooClose reports whether an instant sits inside the minimum gap of one already
// placed. An exact repeat is caught by this too, at any gap above zero, and by
// the equality test when the gap is zero.
func tooClose(at time.Time, placed []time.Time, gap time.Duration) bool {
	for _, p := range placed {
		if at.Equal(p) {
			return true
		}
		if d := at.Sub(p); d < 0 && -d < gap || d >= 0 && d < gap {
			return true
		}
	}
	return false
}

// dates walks the recurrence rule across the run's window and returns the days
// it names — days only, with the time of day thrown away.
//
// The rule is expanded in UTC, from a DTSTART built out of the item's creation
// date, and never sees the item's timezone. That is deliberate on two counts.
// RRULE expresses which days rather than when on them, so a zone has nothing to
// contribute here; and keeping the library's arithmetic away from DST leaves one
// conversion in the system that can get a transition wrong, schedule.Instant,
// which is the one that was written for it (Q-14).
//
// Anchoring on created_at rather than on now is what makes a rule with no BYDAY
// stable. FREQ=WEEKLY means "every seven days from DTSTART", so a DTSTART of
// today would move the item to a different weekday on every nightly run — an
// idempotency break that no amount of slot matching would catch, because the
// slots themselves would have moved.
func (rq request) dates() ([]localDate, error) {
	if rq.schedule.RRule == nil {
		return nil, domain.Invalid("field_required", "schedule.rrule",
			"a %s schedule requires rrule, an RRULE naming which days", rq.schedule.Kind)
	}

	r, err := rrule.StrToRRule(*rq.schedule.RRule)
	if err != nil {
		return nil, domain.Invalid("rrule_parses", "schedule.rrule",
			"rrule %q is not a valid recurrence rule: %s", *rq.schedule.RRule, err)
	}
	r.DTStart(dateOf(rq.item.CreatedAt.In(rq.loc)).utc())

	from := dateOf(rq.run.now.In(rq.loc)).utc()
	to := dateOf(rq.run.to.In(rq.loc)).utc()

	occurrences := r.Between(from, to, true)
	dates := make([]localDate, 0, len(occurrences))
	for _, t := range occurrences {
		dates = append(dates, dateOf(t))
	}
	return dates, nil
}

// instant resolves a wall clock against the item's zone, reporting the two DST
// edge cases at debug. They are rare, deliberate, and the only explanation for a
// reminder that moved by an hour, so the line is worth having when it appears.
func (rq request) instant(d schedule.LocalDateTime) time.Time {
	at, fold := schedule.Instant(d, rq.loc)
	if fold != schedule.FoldNone {
		rq.log.Debug("dst boundary",
			"item", rq.item.ID, "local", d.String(), "zone", rq.loc.String(),
			"fold", fold.String(), "instant", domain.FormatTime(at))
	}
	return at
}

// draw picks a uniform time inside the window, on the five-minute grid.
//
// The grid is applied by drawing from the marks rather than by rounding a free
// minute onto them, which keeps the draw uniform and keeps it inside the window:
// rounding 20:58 to the nearest five in a window ending at 21:00 puts it at
// 21:00 and rounding 09:01 puts it at 09:00, and the ends of a window would
// collect twice their share.
func (rq request) draw(w schedule.Window) schedule.LocalTime {
	first := ceilTo(w.Start.Minutes(), slotStep)
	last := floorTo(w.End.Minutes(), slotStep)
	if last < first {
		// Narrower than one step. Validation requires thirty minutes, so this is
		// unreachable from a validated schedule and cheap to be right about.
		return w.Start
	}

	m := first + slotStep*rq.rnd.IntN((last-first)/slotStep+1)
	return schedule.LocalTime{Hour: m / 60, Minute: m % 60}
}

func ceilTo(v, step int) int  { return ((v + step - 1) / step) * step }
func floorTo(v, step int) int { return (v / step) * step }

// wanted reports whether an instant belongs in this run: inside the horizon,
// still ahead, and not inside a pause window.
func (rq request) wanted(at time.Time) bool {
	return rq.run.covers(at) && !rq.run.paused(rq.item, at)
}

// scaleCount is the partial-period rule from
// docs/05-schedule-spec.md#partial-periods: scale the count to the fraction of
// the period that is left, round down, and never go below one.
//
// Rounding down rather than up is what makes a 3-per-week item created on a
// Thursday produce one occurrence for the stub week instead of two crammed into
// what is left of it. Rounding up only ever reaches one on the last day of a
// period, by which point the minimum has taken over anyway.
func scaleCount(count int, usable, full time.Duration) int {
	if usable <= 0 || full <= 0 {
		return 0
	}
	if usable >= full {
		return count
	}
	n := int(math.Floor(float64(count) * usable.Seconds() / full.Seconds()))
	if n < 1 {
		return 1
	}
	return n
}

// firstAt finds an existing row at exactly this instant, which is the whole slot
// predicate for the two kinds whose times are not drawn.
func firstAt(eligible []*domain.Occurrence, at time.Time) *domain.Occurrence {
	for _, occ := range eligible {
		if occ.StartsAt.Equal(at) {
			return occ
		}
	}
	return nil
}

// firstOnDate finds an existing row on this local date whose time the current
// window would still allow. A row outside the window is a leftover from an
// edited schedule and does not fill the date.
func firstOnDate(eligible []*domain.Occurrence, d localDate, loc *time.Location, w schedule.Window) *domain.Occurrence {
	for _, occ := range eligible {
		local := occ.StartsAt.In(loc)
		if dateOf(local) != d {
			continue
		}
		if !withinWindow(local, w) {
			continue
		}
		return occ
	}
	return nil
}

// fits reports whether an instant is one the current fuzzy schedule could have
// produced: an allowed weekday, inside the window.
func fits(at time.Time, loc *time.Location, allowed map[time.Weekday]bool, w schedule.Window) bool {
	local := at.In(loc)
	return allowed[local.Weekday()] && withinWindow(local, w)
}

func withinWindow(local time.Time, w schedule.Window) bool {
	m := local.Hour()*60 + local.Minute()
	return m >= w.Start.Minutes() && m <= w.End.Minutes()
}

// weekdaySet turns the two-letter codes a schedule carries into stdlib
// weekdays. The codes are RRULE's, which is why they are what days_allowed
// holds even though fuzzy has no rule of its own.
func weekdaySet(codes []string) (map[time.Weekday]bool, error) {
	days := map[string]time.Weekday{
		"MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
		"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday, "SU": time.Sunday,
	}

	set := make(map[time.Weekday]bool, len(codes))
	for _, c := range codes {
		wd, ok := days[c]
		if !ok {
			return nil, domain.Invalid("days_allowed_valid", "schedule.days_allowed",
				"%q is not a weekday code; the codes are MO, TU, WE, TH, FR, SA and SU", c)
		}
		set[wd] = true
	}
	return set, nil
}

// kindList renders the four kinds for the error a stored schedule with an
// unknown kind produces. Validation catches these on the way in; this catches a
// column written before a kind was removed, or by hand.
func kindList() string {
	out := ""
	for i, k := range schedule.Kinds {
		if i > 0 {
			out += ", "
		}
		out += string(k)
	}
	return out
}
