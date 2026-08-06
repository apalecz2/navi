package schedule

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"

	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
)

// The thresholds from the validation table in
// docs/05-schedule-spec.md#validation. They are constants rather than
// configuration because they are design facts, the same reason the loop
// intervals are constants in the loop packages.
const (
	// ProducesWithinDays is how far ahead a recurrence must yield something
	// before it is judged to yield nothing at all.
	ProducesWithinDays = 90

	// DensityWindowDays and DensityLimit bound how dense a recurrence may be.
	// Fifty in thirty days is already more than one a day; past that the
	// request is a misparse rather than an intention.
	DensityWindowDays = 30
	DensityLimit      = 50

	// MinWindowMinutes is how narrow a random-placement window may be before it
	// stops being a window.
	MinWindowMinutes = 30

	// MinCount and MaxCount bound a fuzzy count per period.
	MinCount = 1
	MaxCount = 20

	// GapSlack is the factor in the gap-satisfiable check. Requiring
	// count * gap <= period exactly would reject schedules that are tight but
	// placeable; 1.5 admits those and still catches the arithmetic
	// impossibilities.
	GapSlack = 1.5

	// OneOffMaxYears bounds how far out a one_off may sit. Two years is well
	// past anything a reminder is for, and a date beyond it is a typo in the
	// year.
	OneOffMaxYears = 2

	// countCap bounds the counting expansion, so a pathological rule cannot
	// spend the request iterating. Above it the message says "over".
	countCap = 1000
)

// Weekdays is the two-letter day vocabulary, as RRULE spells it. It is the set
// days_allowed draws from.
var Weekdays = []string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"}

func isWeekday(s string) bool {
	for _, d := range Weekdays {
		if d == s {
			return true
		}
	}
	return false
}

// fields is the set of schedule fields each kind carries. Every field a kind
// carries is required, and every field it does not carry is rejected — after
// Resolve there is no third category, which is what lets one table drive both
// checks and keeps them from disagreeing.
//
// The descriptions are the second half of the required-field message. They are
// here rather than inline so that "a fixed schedule requires at, a local HH:MM
// time" and the fuzzy version of the same sentence are visibly one thing.
var fields = map[Kind]map[string]string{
	KindOneOff: {
		"at": "a local date-time like 2026-08-14T10:00:00",
	},
	KindFixed: {
		"rrule": "an RRULE naming which days",
		"at":    "a local HH:MM time",
	},
	KindWindowed: {
		"rrule":  "an RRULE naming which days",
		"window": "two local HH:MM times",
	},
	KindFuzzy: {
		"period":        "one of day, week or month",
		"count":         "how many times per period",
		"days_allowed":  "the weekdays it may land on",
		"window":        "two local HH:MM times",
		"min_gap_hours": "the minimum hours between occurrences",
	},
}

// fieldOrder is the order the required-field check reports in, so a schedule
// missing two fields names the same one every time.
var fieldOrder = []string{"rrule", "at", "period", "count", "days_allowed", "window", "min_gap_hours"}

// present reports which wire fields a schedule actually carries.
func (s Schedule) present() map[string]bool {
	return map[string]bool{
		"rrule":         s.RRule != nil,
		"at":            s.At != nil,
		"period":        s.Period != nil,
		"count":         s.Count != nil,
		"days_allowed":  len(s.DaysAllowed) > 0,
		"window":        len(s.Window) > 0,
		"min_gap_hours": s.MinGapHours != nil,
	}
}

// Validate runs every row of the table in docs/05-schedule-spec.md#validation
// against a schedule, and returns the first failure as a *domain.ValidationError
// naming the rule and the offending values.
//
// It returns the first failure rather than all of them because the error shape
// in docs/07-api-spec.md is singular and because one precise sentence retries
// better than a concatenation: the model is being asked to fix a thing, not to
// audit its output.
//
// Run it after Resolve. The gap check needs the min_gap_hours the table
// supplies, and the required-field checks assume the window and the day set are
// already filled.
//
// ref is the instant to judge "in the future" against, and loc is the item's
// resolved location from Zones.For.
func Validate(s Schedule, ref time.Time, loc *time.Location) error {
	if loc == nil {
		return fmt.Errorf("schedule: validate: no location")
	}

	if !s.Kind.Valid() {
		return domain.Invalid("schedule_kind", "schedule.kind",
			"kind %q is not one of %s", s.Kind, kindList())
	}

	// The period is checked before the field checks rather than with the rest
	// of the fuzzy rules, because it selects which default gap applies. A
	// schedule saying "fortnight" gets no gap from the table, and without this
	// the first thing it hears is that min_gap_hours is missing — an answer to
	// a question it did not ask.
	if s.Period != nil && !s.Period.Valid() {
		return domain.Invalid("period_valid", "schedule.period",
			"period %q is not %s", *s.Period, periodList())
	}

	// Shape before content: a window on a one_off is a different mistake from a
	// malformed window, and saying so is what makes the retry land.
	if err := s.checkFields(); err != nil {
		return err
	}

	if s.Kind.UsesRRule() {
		if err := s.checkRRule(ref); err != nil {
			return err
		}
	}
	if s.Kind.UsesWindow() {
		if err := s.checkWindow(); err != nil {
			return err
		}
	}

	switch s.Kind {
	case KindOneOff:
		return s.checkOneOff(ref, loc)
	case KindFuzzy:
		return s.checkFuzzy()
	}
	return nil
}

// checkFields rejects fields the kind does not carry and fields it carries and
// is missing.
func (s Schedule) checkFields() error {
	allowed := fields[s.Kind]
	have := s.present()

	for _, name := range fieldOrder {
		if have[name] {
			if _, ok := allowed[name]; !ok {
				return domain.Invalid("field_not_allowed", "schedule."+name,
					"%s is not a field of a %s schedule", name, s.Kind)
			}
		}
	}
	for _, name := range fieldOrder {
		describe, ok := allowed[name]
		if ok && !have[name] {
			return domain.Invalid("field_required", "schedule."+name,
				"a %s schedule requires %s, %s", s.Kind, name, describe)
		}
	}
	return nil
}

// checkRRule covers the three RRULE rows: it parses, it yields something inside
// ninety days, and it is not absurdly dense.
//
// The expansion here counts and does not place. DTSTART is pinned to the
// reference instant in UTC with no timezone conversion and no DST handling,
// because the questions are "does this ever fire" and "does this fire far too
// often", and neither one moves by an hour.
func (s Schedule) checkRRule(ref time.Time) error {
	text := *s.RRule

	r, err := rrule.StrToRRule(text)
	if err != nil {
		return domain.Invalid("rrule_parses", "schedule.rrule",
			"rrule %q is not a valid RRULE: %s", text, err)
	}
	r.DTStart(ref.UTC().Truncate(time.Second))

	if n, _ := countBetween(r, ref, ref.AddDate(0, 0, ProducesWithinDays)); n == 0 {
		return domain.Invalid("rrule_produces_occurrences", "schedule.rrule",
			"rrule %q produces no occurrences in the next %d days", text, ProducesWithinDays)
	}

	n, capped := countBetween(r, ref, ref.AddDate(0, 0, DensityWindowDays))
	if n >= DensityLimit {
		count := fmt.Sprintf("%d", n)
		if capped {
			count = fmt.Sprintf("over %d", countCap)
		}
		return domain.Invalid("rrule_density", "schedule.rrule",
			"rrule %q produces %s occurrences in %d days, over the limit of %d",
			text, count, DensityWindowDays, DensityLimit)
	}
	return nil
}

// countBetween counts occurrences in a range, stopping at countCap so a rule
// like FREQ=SECONDLY costs a thousand iterations rather than millions. It
// reports whether it stopped early.
func countBetween(r *rrule.RRule, from, to time.Time) (int, bool) {
	next := r.Iterator()
	n := 0
	for {
		t, ok := next()
		if !ok || t.After(to) {
			return n, false
		}
		if t.Before(from) {
			continue
		}
		n++
		if n >= countCap {
			return n, true
		}
	}
}

// checkWindow covers the two window rows: ordered, and at least half an hour
// wide. Shape and HH:MM format come from WindowTimes, which is the same parse
// the materializer uses.
func (s Schedule) checkWindow() error {
	w, err := s.WindowTimes()
	if err != nil {
		return err
	}
	if w.Start.Minutes() >= w.End.Minutes() {
		return domain.Invalid("window_ordered", "schedule.window",
			"window start %s is not before window end %s", w.Start, w.End)
	}
	if w.Width() < MinWindowMinutes {
		return domain.Invalid("window_width", "schedule.window",
			"window %s is %d minutes wide, under the %d minute minimum",
			w, w.Width(), MinWindowMinutes)
	}
	return nil
}

// checkOneOff covers the two one_off rows: in the future, and under two years
// out.
func (s Schedule) checkOneOff(ref time.Time, loc *time.Location) error {
	at, err := s.OneOffAt()
	if err != nil {
		return err
	}
	instant := at.In(loc)

	if !instant.After(ref) {
		return domain.Invalid("one_off_future", "schedule.at",
			"one_off at %s is in the past (now is %s in %s)",
			at, ref.In(loc).Format("2006-01-02T15:04"), loc)
	}
	if instant.After(ref.AddDate(OneOffMaxYears, 0, 0)) {
		return domain.Invalid("one_off_bounded", "schedule.at",
			"one_off at %s is more than %d years out", at, OneOffMaxYears)
	}
	return nil
}

// checkFuzzy covers the count, day-set and gap rows. The period was checked in
// Validate, ahead of the field checks. The gap is last because it is the
// interesting one and it reads best when everything it names has already been
// established as sane.
func (s Schedule) checkFuzzy() error {
	period := *s.Period

	count := *s.Count
	if count < MinCount || count > MaxCount {
		return domain.Invalid("count_range", "schedule.count",
			"count %d is outside the allowed range of %d to %d per period",
			count, MinCount, MaxCount)
	}

	for i, day := range s.DaysAllowed {
		if !isWeekday(day) {
			return domain.Invalid("days_allowed", fmt.Sprintf("schedule.days_allowed[%d]", i),
				"days_allowed entry %q is not a two-letter weekday (%s)",
				day, joinWeekdays())
		}
	}

	gap := *s.MinGapHours
	if gap < 0 {
		return domain.Invalid("min_gap_range", "schedule.min_gap_hours",
			"min_gap_hours %d is negative", gap)
	}

	// The check the whole kind hangs on. "Five times a day, at least eight
	// hours apart" is arithmetically impossible, and without this it would burn
	// two hundred placement attempts in the materializer and then quietly
	// produce three.
	periodHours, _ := period.Hours()
	if float64(count*gap) > float64(periodHours)*GapSlack {
		return domain.Invalid("gap_satisfiable", "schedule.min_gap_hours",
			"min_gap_hours %d cannot be satisfied with count %d over period '%s'",
			gap, count, period)
	}
	return nil
}

func joinWeekdays() string { return strings.Join(Weekdays, ", ") }

// Prepare is the write path in one call: decode, fill from the table, validate.
// It returns the schedule as it will be stored and the list of fields it
// inferred, which A5 turns into the "I assumed" clause of the confirmation.
//
// The three steps stay separate functions because the read path wants only the
// first, and because keeping Resolve out of Parse is what stops a stored
// schedule from acquiring fields on its way out of the column.
func Prepare(raw json.RawMessage, table *defaults.Table, ref time.Time, loc *time.Location) (Schedule, []Inference, error) {
	s, err := Parse(raw)
	if err != nil {
		return Schedule{}, nil, err
	}
	if !s.Kind.Valid() {
		// Resolve branches on the kind, so an unknown one is caught before it
		// rather than silently filling nothing and failing later with a
		// message about a missing window.
		return Schedule{}, nil, domain.Invalid("schedule_kind", "schedule.kind",
			"kind %q is not one of %s", s.Kind, kindList())
	}
	s, inferences, err := Resolve(s, table)
	if err != nil {
		return Schedule{}, nil, err
	}
	if err := Validate(s, ref, loc); err != nil {
		return Schedule{}, inferences, err
	}
	return s, inferences, nil
}

// CheckTable validates the schedule vocabulary inside defaults.yaml: that every
// window is two HH:MM times in the right order and wide enough, and that every
// default day is a weekday token.
//
// It lives here rather than in the defaults package because these are the
// schedule's rules, and a table that satisfied the loader but produced
// schedules the validator rejects would be the worst of both. Checking at
// startup means a typo in the file is a failed boot rather than a rejected
// reminder three days later.
func CheckTable(t *defaults.Table) error {
	for name, w := range t.Windows {
		field := "windows." + name
		start, err := ParseLocalTime(field, "window start", w.Start)
		if err != nil {
			return err
		}
		end, err := ParseLocalTime(field, "window end", w.End)
		if err != nil {
			return err
		}
		if start.Minutes() >= end.Minutes() {
			return domain.Invalid("window_ordered", field,
				"window %q starts at %s and ends at %s, which is not in order", name, start, end)
		}
		if end.Minutes()-start.Minutes() < MinWindowMinutes {
			return domain.Invalid("window_width", field,
				"window %q is %d minutes wide, under the %d minute minimum",
				name, end.Minutes()-start.Minutes(), MinWindowMinutes)
		}
	}

	for i, day := range t.Defaults.DaysAllowed {
		if !isWeekday(day) {
			return domain.Invalid("days_allowed", fmt.Sprintf("defaults.days_allowed[%d]", i),
				"days_allowed entry %q is not a two-letter weekday (%s)", day, joinWeekdays())
		}
	}
	return nil
}
