package domain

import (
	"fmt"
	"time"
)

// TimeLayout is how every timestamp in the database is written: ISO-8601 UTC
// with a Z suffix and second precision.
//
// The precision and the fixed layout are load-bearing rather than tidy. SQLite
// has no timestamp type, so these are TEXT columns and every comparison the
// scheduler makes is a string comparison — starts_at <= ? sorts correctly only
// while every row is the same width and the same zone. One row written with a
// fractional second or a numeric offset sorts wrong from then on, and it sorts
// wrong quietly.
//
// The trailing Z is a literal in this layout, not Go's Z07:00 zone verb, so
// parsing rejects a value carrying an offset instead of silently accepting it.
const TimeLayout = "2006-01-02T15:04:05Z"

// DateLayout is the local calendar date, used for kv keys that are scoped to a
// day rather than an instant (proactive_count:{date}, last_reconcile_date).
const DateLayout = "2006-01-02"

// FormatTime renders a time for storage. It converts to UTC and truncates to
// the second, so the caller cannot produce a value that breaks text ordering.
func FormatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(TimeLayout)
}

// ParseTime reads a stored timestamp. The returned time is in UTC.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("domain: parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}
