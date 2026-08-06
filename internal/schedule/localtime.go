package schedule

import (
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// A schedule holds two kinds of local time and neither is an instant. A time of
// day is "09:00" and means that wall clock on whichever day the RRULE picks; a
// one_off's `at` is "2026-08-14T10:00:00" and means that wall clock on that
// date. Both become UTC instants at materialization, against the item's
// timezone, and not before.
//
// The wire fields stay strings on Schedule and are parsed here, because the
// error a malformed one produces has to name the field path — "window start" is
// not something a LocalTime knows about itself.

// LocalTimeLayout is a time of day, zero-padded. Parsing is strict about the
// padding so that what lands in the column is one canonical form: the value is
// stored as the string it arrived as, and a mix of "9:00" and "09:00" in the
// database is a sorting and diffing problem for no gain.
const LocalTimeLayout = "15:04"

// LocalDateTimeLayout is the naive local date-time a one_off carries. The
// seconds-less form is accepted too, since models emit it and it is
// unambiguous.
const (
	LocalDateTimeLayout       = "2006-01-02T15:04:05"
	LocalDateTimeLayoutNoSecs = "2006-01-02T15:04"
)

// LocalTime is a time of day with no date and no zone.
type LocalTime struct {
	Hour   int
	Minute int
}

// ParseLocalTime reads an HH:MM value. field is the dotted path used in the
// error, and label names the value in the message — "window start" reads better
// than "schedule.window[0]" in a sentence, and both end up in the error.
func ParseLocalTime(field, label, value string) (LocalTime, error) {
	t, err := time.Parse(LocalTimeLayout, value)
	if err != nil {
		return LocalTime{}, domain.Invalid("local_time_format", field,
			"%s %q is not an HH:MM local time", label, value)
	}
	return LocalTime{Hour: t.Hour(), Minute: t.Minute()}, nil
}

// Minutes is the time of day as minutes since midnight, which is what ordering
// and width comparisons want.
func (t LocalTime) Minutes() int { return t.Hour*60 + t.Minute }

// String renders the canonical HH:MM form.
func (t LocalTime) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// Window is a parsed time-of-day range.
type Window struct {
	Start LocalTime
	End   LocalTime
}

// Width is the span in minutes.
func (w Window) Width() int { return w.End.Minutes() - w.Start.Minutes() }

// String renders the range for a message or an inference.
func (w Window) String() string { return w.Start.String() + "-" + w.End.String() }

// LocalDateTime is a wall clock on a date, with no zone attached.
type LocalDateTime struct {
	Year   int
	Month  time.Month
	Day    int
	Hour   int
	Minute int
	Second int
}

// ParseLocalDateTime reads a naive local date-time.
func ParseLocalDateTime(field, label, value string) (LocalDateTime, error) {
	for _, layout := range []string{LocalDateTimeLayout, LocalDateTimeLayoutNoSecs} {
		t, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		return LocalDateTime{
			Year: t.Year(), Month: t.Month(), Day: t.Day(),
			Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
		}, nil
	}
	return LocalDateTime{}, domain.Invalid("local_datetime_format", field,
		"%s %q is not a naive local date-time like 2026-08-14T10:00:00", label, value)
}

// String renders the canonical form.
func (d LocalDateTime) String() string {
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d",
		d.Year, int(d.Month), d.Day, d.Hour, d.Minute, d.Second)
}

// In places the wall clock in a location with time.Date and nothing more.
//
// This is deliberately not the DST-correct conversion. docs/05-schedule-spec.md
// requires a nonexistent local time to shift forward to the first valid one and
// an ambiguous one to take the earlier offset, and time.Date does the first by
// accident and the second unpredictably. That handling belongs with the
// materializer, which is the only thing that writes an instant to a row.
//
// Validation is the one caller allowed to use this, because the questions it
// asks — is this in the future, is it under two years out — cannot be changed
// by an hour of slop at a DST boundary.
func (d LocalDateTime) In(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, d.Hour, d.Minute, d.Second, 0, loc)
}

// OneOffAt parses a one_off's `at`. It is an error to call this on any other
// kind, and on a schedule that has not been validated it can fail; after
// validation it cannot.
func (s Schedule) OneOffAt() (LocalDateTime, error) {
	if s.Kind != KindOneOff {
		return LocalDateTime{}, fmt.Errorf("schedule: one-off time requested from a %s schedule", s.Kind)
	}
	if s.At == nil {
		return LocalDateTime{}, domain.Invalid("field_required", "schedule.at",
			"a one_off schedule requires at, a local date-time like 2026-08-14T10:00:00")
	}
	return ParseLocalDateTime("schedule.at", "at", *s.At)
}

// TimeOfDay parses a fixed schedule's `at`.
func (s Schedule) TimeOfDay() (LocalTime, error) {
	if s.Kind != KindFixed {
		return LocalTime{}, fmt.Errorf("schedule: time of day requested from a %s schedule", s.Kind)
	}
	if s.At == nil {
		return LocalTime{}, domain.Invalid("field_required", "schedule.at",
			"a fixed schedule requires at, a local HH:MM time")
	}
	return ParseLocalTime("schedule.at", "at", *s.At)
}

// WindowTimes parses the window of a windowed or fuzzy schedule. After Resolve
// every such schedule has one, so the materializer can treat a failure here as
// a bug rather than as input.
func (s Schedule) WindowTimes() (Window, error) {
	if !s.Kind.UsesWindow() {
		return Window{}, fmt.Errorf("schedule: window requested from a %s schedule", s.Kind)
	}
	if len(s.Window) != 2 {
		return Window{}, domain.Invalid("window_shape", "schedule.window",
			"window must be exactly two HH:MM times, got %d", len(s.Window))
	}
	start, err := ParseLocalTime("schedule.window[0]", "window start", s.Window[0])
	if err != nil {
		return Window{}, err
	}
	end, err := ParseLocalTime("schedule.window[1]", "window end", s.Window[1])
	if err != nil {
		return Window{}, err
	}
	return Window{Start: start, End: end}, nil
}
