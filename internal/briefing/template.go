package briefing

import (
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/store"
)

// composeTemplate renders the briefing as a plain deterministic summary, with
// no model call — the fallback O7 requires and the live path for any deployment
// with no chat transport. It cannot get the tone wrong, so it is safe to reach
// for at the moment there is no way to check.
//
// Same facts, same order as the model is given (renderContextForModel), so a
// fallback mid-window does not change which items or goals are named. Nothing
// here counts misses or comments on the day — persona.md's G8 binds this text
// the same way it binds reminder copy.
func composeTemplate(blob Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.\n", blob.Local.Format("Monday, 2 January"))
	b.WriteString(renderOccLines(blob.Occs))

	if goals := renderGoalLines(blob.Goals); goals != "" {
		b.WriteString(goals)
	}

	b.WriteString("Anything else you want today to include?")
	return b.String()
}

// renderOccLines is today's occurrences as one sentence, silent ones flagged so
// "it won't ping" is stated rather than left to be discovered.
func renderOccLines(occs []occView) string {
	pending := make([]string, 0, len(occs))
	var silent []string
	for _, o := range occs {
		if o.Done {
			continue
		}
		if o.Silent {
			silent = append(silent, o.Title)
			continue
		}
		pending = append(pending, fmt.Sprintf("%s (%s)", o.Title, o.Time))
	}

	var b strings.Builder
	switch len(pending) {
	case 0:
		b.WriteString("Nothing on the schedule that will ping today.\n")
	case 1:
		fmt.Fprintf(&b, "On deck today: %s.\n", pending[0])
	default:
		fmt.Fprintf(&b, "On deck today: %s.\n", strings.Join(pending, ", "))
	}
	if len(silent) > 0 {
		fmt.Fprintf(&b, "Silent, so no ping: %s.\n", joinAnd(silent))
	}
	return b.String()
}

// renderGoalLines is the active goals and where each stands for its period.
// Empty string when there are no goals, so composeTemplate omits the line
// entirely rather than printing a heading with nothing under it.
func renderGoalLines(goals []store.GoalProgress) string {
	if len(goals) == 0 {
		return ""
	}
	parts := make([]string, 0, len(goals))
	for _, g := range goals {
		switch {
		case g.ItemLinked:
			s := fmt.Sprintf("%s at %d of %d this period", g.Goal.Title, g.Completed, g.Target)
			if g.Completed < g.Target {
				s += " (behind pace)"
			}
			parts = append(parts, s)
		case g.HasUpdate && g.LatestPct != nil:
			parts = append(parts, fmt.Sprintf("%s at %d%%", g.Goal.Title, *g.LatestPct))
		default:
			parts = append(parts, fmt.Sprintf("%s in progress", g.Goal.Title))
		}
	}
	return "Goals: " + strings.Join(parts, "; ") + ".\n"
}

// joinAnd renders a short list the way a person writes one: "a", "a and b",
// "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
