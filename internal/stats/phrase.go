package stats

import (
	"fmt"
	"strings"
)

// The words a figure is spoken in. They live here, beside the figures, so the
// dashboard and the get_stats confirmation say a number the same way: they
// format what Service computed and are the only formatting both surfaces share.

// Percent renders a completion rate as a whole percent.
func Percent(rate float64) string { return fmt.Sprintf("%.0f%%", rate*100) }

// MinutesPhrase renders a median lag, in minutes below two hours and in hours
// above.
func MinutesPhrase(m float64) string {
	if m >= 120 {
		return fmt.Sprintf("%.1f hours", m/60)
	}
	return fmt.Sprintf("%.0f min", m)
}

// StreakPhrase is the one wording of a streak, on the page and in the chat:
// "12 in a row (best 20)".
//
// Not "12 days" and not "12 weekdays". A streak counts chains, and a chain exists
// only where the schedule produced one (docs/11-goals-spec.md, weekday-aware
// streaks), so the unit is the item's own cadence: a Tuesday/Thursday item's 12
// is twelve sessions over six weeks, a weekday item's 12 is two and a half weeks.
// "Weekdays" would be true for the second and wrong for every other schedule, and
// "days" would be wrong for both. "In a row" is true for all of them and states
// the one thing a streak claims: no miss in between.
func StreakPhrase(current, longest int) string {
	return fmt.Sprintf("%d in a row (best %d)", current, longest)
}

// GoalPhrase is a goal's progress as one clause: "2 of 3, 2.0 a week" for an
// item-linked goal, "60%" for a freestanding one. The status is the caller's to
// add, since the page shows it as a tag and the chat as a suffix.
func GoalPhrase(g GoalStats) string {
	switch {
	case g.Completed != nil && g.Target != nil:
		out := fmt.Sprintf("%d of %d", *g.Completed, *g.Target)
		if g.Velocity != nil {
			out += fmt.Sprintf(", %.1f a week", *g.Velocity)
		}
		return out
	case g.ProgressPct != nil:
		return fmt.Sprintf("%d%%", *g.ProgressPct)
	default:
		return "no progress logged"
	}
}

// RateDetail is the sentence under the headline rate: what the percentage is
// made of, or why there is none. The statistics page and the weekly digest both
// say it, so it is here.
func (s Summary) RateDetail() string {
	t := s.Totals
	switch {
	case s.CompletionRate != nil:
		out := fmt.Sprintf("%d of %d completed", t.Completed, t.Settled)
		if rest := t.Others(); rest != "" {
			out += "; " + rest
		}
		return out
	case t.Chains == 0:
		return "nothing was scheduled"
	default:
		return "nothing has settled yet (" + t.Unsettled() + ")"
	}
}

// Meta is one item's line of figures over the window, parts omitted when absent:
// "100% done · median 10 min · skipped: on vacation". The streak is not in it;
// it is lifetime and has its own phrase.
func (it ItemStats) Meta() string {
	var parts []string
	if it.CompletionRate != nil {
		parts = append(parts, Percent(*it.CompletionRate)+" done")
	}
	if it.MedianLagMinutes != nil {
		parts = append(parts, "median "+MinutesPhrase(*it.MedianLagMinutes))
	}
	if len(it.RecentSkipNotes) > 0 {
		parts = append(parts, "skipped: "+strings.Join(it.RecentSkipNotes, "; "))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing settled in this window")
	}
	return strings.Join(parts, " · ")
}

// Others names the chains outside the rate, so a skip or an unanswered
// check-in is visible without being counted as either outcome.
func (t Totals) Others() string {
	var parts []string
	if t.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", t.Skipped))
	}
	if u := t.Unsettled(); u != "" {
		parts = append(parts, u)
	}
	return strings.Join(parts, ", ")
}

func (t Totals) Unsettled() string {
	var parts []string
	if t.Awaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting your answer", t.Awaiting))
	}
	if t.Open > 0 {
		parts = append(parts, fmt.Sprintf("%d still open", t.Open))
	}
	return strings.Join(parts, ", ")
}

// RangePhrase is a range spoken as the opening of a sentence.
func RangePhrase(r string) string {
	switch Range(r) {
	case RangeWeek:
		return "Over the last 7 days"
	case RangeMonth:
		return "Over the last 30 days"
	case RangeQuarter:
		return "Over the last 90 days"
	default:
		return "Across all time"
	}
}
