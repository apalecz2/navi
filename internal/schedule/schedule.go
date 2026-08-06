// Package schedule is the four schedule kinds from
// docs/05-schedule-spec.md: their Go representation, the JSON they round-trip
// through items.schedule, the defaults that fill an under-specified one, the
// semantic validation every one passes before a write, and which timezone an
// item resolves to.
//
// It generates no occurrences. Expanding a schedule into rows — fuzzy
// placement, the random draw, converting a local wall clock to a UTC instant
// and the two DST edge cases — belongs to the materializer, and the only
// expansion here is counting, which two rows of the validation table require.
//
// It is pure logic. The one thing it reads is the defaults table, and that
// arrives as a value.
package schedule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// Kind discriminates the union. The four are not collapsible: one_off is not an
// RRULE with COUNT=1 because its validation rules differ and it never
// re-materializes, and fuzzy has no RRULE representation at all, since RRULE
// expresses which days rather than how many times somewhere in here (D-004).
type Kind string

const (
	KindOneOff   Kind = "one_off"
	KindFixed    Kind = "fixed"
	KindWindowed Kind = "windowed"
	KindFuzzy    Kind = "fuzzy"
)

// Kinds is the set, in the order docs/05-schedule-spec.md introduces them. It
// is what the "not one of" message lists, so the message and the switch cannot
// disagree.
var Kinds = []Kind{KindOneOff, KindFixed, KindWindowed, KindFuzzy}

// Period is the unit a fuzzy count is per.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
)

// Periods is the set, in ascending order.
var Periods = []Period{PeriodDay, PeriodWeek, PeriodMonth}

// Hours is the length of a period, used by the gap-satisfiable check. A month
// is thirty days rather than a calendar month: the check is an arithmetic
// sanity test, not a placement, and the materializer's real month boundaries
// have no bearing on whether five-times-a-day-eight-hours-apart is nonsense.
func (p Period) Hours() (int, bool) {
	switch p {
	case PeriodDay:
		return 24, true
	case PeriodWeek:
		return 24 * 7, true
	case PeriodMonth:
		return 24 * 30, true
	default:
		return 0, false
	}
}

// Schedule is the tagged union stored as JSON in items.schedule.
//
// It is one flat struct with a discriminator rather than four variant structs
// behind an envelope, for three reasons.
//
// The wire format is flat, so `omitempty` alone marshals it: the fields are
// declared in the order docs/05-schedule-spec.md writes its examples, and all
// four round-trip byte for byte with no custom MarshalJSON to keep in step.
//
// P1 mirrors this struct in the agent's tool arguments and generates the
// model-facing JSON Schema from it. One flat object with a discriminator is a
// schema a model fills reliably; a variant union is a hand-written oneOf.
//
// And the combinations this shape allows and a variant union would not — rrule
// on a fuzzy schedule, a window on a one_off — are better caught by the
// validator than by the type system, because the validator's answer is a
// sentence naming the field, and that sentence is what gets fed back to the
// model on retry. A decode failure is not.
//
// Every optional field is a pointer or a slice for the reason recorded in
// docs/06-agent-spec.md: 0 and omitted are the same value for a plain int, and
// count has no safe default, so "absent" has to be representable.
type Schedule struct {
	Kind Kind `json:"kind"`

	// RRule is fixed and windowed: which days. No DTSTART — that comes from the
	// materialization window, not from the stored rule.
	RRule *string `json:"rrule,omitempty"`

	// At is one_off and fixed, and it means something different in each: a
	// naive local date-time for one_off, a local HH:MM for fixed. One key, two
	// meanings, because that is what the spec's JSON does. Read it through
	// OneOffAt or TimeOfDay rather than directly.
	At *string `json:"at,omitempty"`

	// Period, Count, DaysAllowed and MinGapHours are fuzzy.
	Period      *Period  `json:"period,omitempty"`
	Count       *int     `json:"count,omitempty"`
	DaysAllowed []string `json:"days_allowed,omitempty"`

	// Window is windowed and fuzzy: two local HH:MM times, start then end.
	Window []string `json:"window,omitempty"`

	MinGapHours *int `json:"min_gap_hours,omitempty"`
}

// shadow exists only so UnmarshalJSON can decode without recursing into
// itself.
type shadow Schedule

// UnmarshalJSON decodes with DisallowUnknownFields.
//
// Strictness here is worth the custom method. The alternative is that a model
// emitting "windows" or "min_gap" gets a schedule that parses cleanly, is
// missing the field it thought it set, and then either fails a required-field
// check that names the wrong problem or quietly picks up a default. Naming the
// unknown key is the difference between one retry that fixes it and three that
// do not.
func (s *Schedule) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var sh shadow
	if err := dec.Decode(&sh); err != nil {
		return decodeError(err)
	}
	*s = Schedule(sh)
	return nil
}

// decodeError turns encoding/json's wording into the shape the escalation
// ladder wants. The unknown-field case is the one worth translating, because it
// is the one a model causes and the one it can fix.
func decodeError(err error) error {
	msg := err.Error()
	const unknown = "json: unknown field "
	if i := strings.Index(msg, unknown); i >= 0 {
		field := strings.Trim(msg[i+len(unknown):], `"`)
		return domain.Invalid("unknown_field", "schedule."+field,
			"%q is not a field of any schedule kind; the fields are kind, rrule, at, period, count, days_allowed, window and min_gap_hours",
			field)
	}
	return domain.Invalid("schedule_json", "schedule", "schedule is not valid JSON: %s", msg)
}

// Parse decodes a stored schedule exactly as it is. It applies no defaults: the
// read path must not invent fields the column does not hold, and the write path
// calls Resolve for that in a separate pass.
func Parse(raw json.RawMessage) (Schedule, error) {
	if len(raw) == 0 {
		return Schedule{}, domain.Invalid("schedule_required", "schedule", "schedule is required")
	}
	var s Schedule
	if err := json.Unmarshal(raw, &s); err != nil {
		return Schedule{}, err
	}
	return s, nil
}

// Marshal renders a schedule for storage. Field order is declaration order, so
// a schedule read from the column and written back is unchanged.
func (s Schedule) Marshal() (json.RawMessage, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("schedule: marshal: %w", err)
	}
	return data, nil
}

// String renders a schedule for a log line or a confirmation. It is the
// compact form the agent's context injection uses — "fuzzy 3/week, window
// 09:00-21:00" rather than the JSON.
func (s Schedule) String() string {
	var b strings.Builder
	b.WriteString(string(s.Kind))
	switch s.Kind {
	case KindOneOff:
		if s.At != nil {
			fmt.Fprintf(&b, " at %s", *s.At)
		}
	case KindFixed:
		if s.RRule != nil {
			fmt.Fprintf(&b, " %s", *s.RRule)
		}
		if s.At != nil {
			fmt.Fprintf(&b, " at %s", *s.At)
		}
	case KindWindowed:
		if s.RRule != nil {
			fmt.Fprintf(&b, " %s", *s.RRule)
		}
		if len(s.Window) == 2 {
			fmt.Fprintf(&b, ", window %s-%s", s.Window[0], s.Window[1])
		}
	case KindFuzzy:
		if s.Count != nil && s.Period != nil {
			fmt.Fprintf(&b, " %d/%s", *s.Count, *s.Period)
		}
		if len(s.DaysAllowed) > 0 {
			fmt.Fprintf(&b, ", days %s", strings.Join(s.DaysAllowed, ","))
		}
		if len(s.Window) == 2 {
			fmt.Fprintf(&b, ", window %s-%s", s.Window[0], s.Window[1])
		}
		if s.MinGapHours != nil {
			fmt.Fprintf(&b, ", gap %dh", *s.MinGapHours)
		}
	}
	return b.String()
}

// UsesRRule reports whether the kind carries a recurrence rule. The two that do
// are also the two that export to a calendar as recurring VEVENTs (X2).
func (k Kind) UsesRRule() bool { return k == KindFixed || k == KindWindowed }

// UsesWindow reports whether the kind draws a random time inside a range.
func (k Kind) UsesWindow() bool { return k == KindWindowed || k == KindFuzzy }

// Valid reports whether a kind is one of the four.
func (k Kind) Valid() bool {
	switch k {
	case KindOneOff, KindFixed, KindWindowed, KindFuzzy:
		return true
	}
	return false
}

// Valid reports whether a period is one of the three.
func (p Period) Valid() bool {
	_, ok := p.Hours()
	return ok
}

// kindList renders the four kinds for an error message.
func kindList() string {
	names := make([]string, len(Kinds))
	for i, k := range Kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// periodList renders the three periods for an error message, joined with "or"
// so the sentence reads as a sentence.
func periodList() string {
	names := make([]string, len(Periods))
	for i, p := range Periods {
		names[i] = string(p)
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
