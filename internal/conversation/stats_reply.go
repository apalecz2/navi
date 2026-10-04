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
	b.WriteString(stats.RangePhrase(s.Range))

	t := s.Totals
	switch {
	case t.Chains == 0:
		b.WriteString(": nothing was scheduled.")
	case s.CompletionRate == nil:
		fmt.Fprintf(&b, ": nothing has settled yet (%s).", t.Unsettled())
	default:
		fmt.Fprintf(&b, ": %d of %d completed (%s)", t.Completed, t.Settled, stats.Percent(*s.CompletionRate))
		if rest := t.Others(); rest != "" {
			b.WriteString("; " + rest)
		}
		b.WriteString(".")
	}
	if s.MedianLagMinutes != nil {
		fmt.Fprintf(&b, " Median %s from reminder to done.", stats.MinutesPhrase(*s.MedianLagMinutes))
	}

	for _, it := range s.Items {
		line := fmt.Sprintf("\n- %s", it.Title)
		if it.Archived {
			line += " (archived)"
		}
		line += ": " + stats.StreakPhrase(it.CurrentStreak, it.LongestStreak)
		if it.CompletionRate != nil {
			line += ", " + stats.Percent(*it.CompletionRate)
		}
		if len(it.RecentSkipNotes) > 0 {
			line += fmt.Sprintf(", skipped: %s", strings.Join(it.RecentSkipNotes, "; "))
		}
		b.WriteString(line)
	}

	for _, g := range s.Goals {
		fmt.Fprintf(&b, "\n- goal %q: %s", g.Title, stats.GoalPhrase(g))
		if g.Status != "active" {
			fmt.Fprintf(&b, " (%s)", g.Status)
		}
	}
	return b.String()
}
