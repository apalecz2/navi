package conversation

import (
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/stats"
)

// renderStats is get_stats' reply. It phrases a stats.Summary and nothing more:
// every figure below is read off a field, none is derived here, which is what
// keeps the chat's numbers identical to /api/stats/summary's (V6). If a figure
// is wanted that the summary does not carry, it is added to internal/stats, not
// worked out in this file.
func renderStats(s *stats.Summary) string {
	if s == nil {
		return "I couldn't read your statistics."
	}

	var b strings.Builder
	b.WriteString(statsRangePhrase(s.Range))

	t := s.Totals
	switch {
	case t.Chains == 0:
		b.WriteString(": nothing was scheduled.")
	case s.CompletionRate == nil:
		fmt.Fprintf(&b, ": nothing has settled yet (%s).", statsUnsettled(t))
	default:
		fmt.Fprintf(&b, ": %d of %d completed (%s)", t.Completed, t.Settled, percent(*s.CompletionRate))
		if rest := statsOthers(t); rest != "" {
			b.WriteString("; " + rest)
		}
		b.WriteString(".")
	}
	if s.MedianLagMinutes != nil {
		fmt.Fprintf(&b, " Median %s from reminder to done.", minutesPhrase(*s.MedianLagMinutes))
	}

	for _, it := range s.Items {
		line := fmt.Sprintf("\n- %s", it.Title)
		if it.Archived {
			line += " (archived)"
		}
		line += fmt.Sprintf(": streak %d (best %d)", it.CurrentStreak, it.LongestStreak)
		if it.CompletionRate != nil {
			line += ", " + percent(*it.CompletionRate)
		}
		if len(it.RecentSkipNotes) > 0 {
			line += fmt.Sprintf(", skipped: %s", strings.Join(it.RecentSkipNotes, "; "))
		}
		b.WriteString(line)
	}

	for _, g := range s.Goals {
		switch {
		case g.Completed != nil && g.Target != nil:
			fmt.Fprintf(&b, "\n- goal %q: %d of %d", g.Title, *g.Completed, *g.Target)
			if g.Velocity != nil {
				fmt.Fprintf(&b, ", %.1f a week", *g.Velocity)
			}
		case g.ProgressPct != nil:
			fmt.Fprintf(&b, "\n- goal %q: %d%%", g.Title, *g.ProgressPct)
		default:
			fmt.Fprintf(&b, "\n- goal %q: no progress logged", g.Title)
		}
		if g.Status != "active" {
			fmt.Fprintf(&b, " (%s)", g.Status)
		}
	}
	return b.String()
}

func statsRangePhrase(r string) string {
	switch stats.Range(r) {
	case stats.RangeWeek:
		return "Over the last 7 days"
	case stats.RangeMonth:
		return "Over the last 30 days"
	case stats.RangeQuarter:
		return "Over the last 90 days"
	default:
		return "Across all time"
	}
}

// statsOthers names the chains outside the rate, so a skip or an unanswered
// check-in is visible without being counted as either outcome.
func statsOthers(t stats.Totals) string {
	var parts []string
	if t.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", t.Skipped))
	}
	if u := statsUnsettled(t); u != "" {
		parts = append(parts, u)
	}
	return strings.Join(parts, ", ")
}

func statsUnsettled(t stats.Totals) string {
	var parts []string
	if t.Awaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting your answer", t.Awaiting))
	}
	if t.Open > 0 {
		parts = append(parts, fmt.Sprintf("%d still open", t.Open))
	}
	return strings.Join(parts, ", ")
}

func percent(rate float64) string { return fmt.Sprintf("%.0f%%", rate*100) }

func minutesPhrase(m float64) string {
	if m >= 120 {
		return fmt.Sprintf("%.1f hours", m/60)
	}
	return fmt.Sprintf("%.0f min", m)
}
