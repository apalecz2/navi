package feed

import (
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/ical"
)

// isUTC reports whether a zone is written as a Z suffix instead of a TZID. UTC
// has no transitions to describe, and "TZID=UTC" is a name some clients do not
// resolve.
func isUTC(loc *time.Location) bool { return loc == time.UTC || loc.String() == "UTC" }

// localStamp is a wall clock in iCalendar's floating form, YYYYMMDDTHHMMSS.
func localStamp(t time.Time) string { return t.Format("20060102T150405") }

// utcStamp is an instant with the Z suffix.
func utcStamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// offsetString is a UTC offset as ±HHMM, or ±HHMMSS when it has seconds.
func offsetString(sec int) string {
	sign := '+'
	if sec < 0 {
		sign, sec = '-', -sec
	}
	h, m, s := sec/3600, sec%3600/60, sec%60
	if s != 0 {
		return fmt.Sprintf("%c%02d%02d%02d", sign, h, m, s)
	}
	return fmt.Sprintf("%c%02d%02d", sign, h, m)
}

// vtimezone describes a zone from the Go tz database for the span the feed
// covers, plus a year before and two after.
//
// Every transition in that span is written out as its own observance rather than
// as a yearly RRULE. A rule would be shorter, and deriving one from the tzdb is
// right for the US and the EU and quietly wrong for the zones whose rules are
// not "nth weekday of a month" (Morocco around Ramadan, Iran's old calendar); a
// list of dated transitions is exactly what the database says and cannot be
// wrong that way. Past the last one, clients fall back on their own tz database
// by TZID, which is why X-LIC-LOCATION is written too: the feed refreshes
// hourly and the span slides with it, so the fallback only ever matters for a
// series recurring beyond it.
func vtimezone(loc *time.Location, from, to time.Time) *ical.Component {
	name := loc.String()
	c := ical.New("VTIMEZONE")
	c.Add("TZID", name)
	c.Add("X-LIC-LOCATION", name)

	start := from.AddDate(-1, 0, 0)
	end := to.AddDate(2, 0, 0)

	cur := start.In(loc)
	obs := func(kind string, wall time.Time, fromOff, toOff int, abbrev string) {
		o := c.Sub(kind)
		o.Add("DTSTART", localStamp(wall))
		o.Add("TZOFFSETFROM", offsetString(fromOff))
		o.Add("TZOFFSETTO", offsetString(toOff))
		o.AddText("TZNAME", abbrev)
	}
	kindOf := func(t time.Time) string {
		if t.IsDST() {
			return "DAYLIGHT"
		}
		return "STANDARD"
	}

	abbrev, off := cur.Zone()
	obs(kindOf(cur), time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), off, off, abbrev)

	for {
		_, next := cur.ZoneBounds()

		// Past the last explicit transition Go synthesizes boundaries from the
		// zone's rule, and two of its habits matter here: it cuts a period at the
		// end of each year with no change of offset, and eventually reports the
		// same boundary again. Neither is a transition, so progress is required
		// and an unchanged offset is skipped.
		if next.IsZero() || !next.Before(end) || !next.After(cur) {
			return c
		}
		_, fromOff := cur.Zone()
		cur = next.In(loc)
		abbrev, toOff := cur.Zone()
		if toOff == fromOff {
			continue
		}

		// The observance's DTSTART is the wall clock in force just before the
		// change, which is the instant read in the old offset.
		wall := next.In(time.FixedZone("", fromOff))
		obs(kindOf(cur), time.Date(wall.Year(), wall.Month(), wall.Day(),
			wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC), fromOff, toOff, abbrev)
	}
}
