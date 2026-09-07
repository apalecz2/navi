package conversation

import (
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// buildConfirmation renders a plain-language reply naming up to the next
// three concrete timestamps and every inferred parameter (A5) straight from
// agent.Result - NextOccurrences is already domain.FormatTime'd and Inferred
// is resolveSchedule's own return value, so neither costs a second model
// call. toolName picks the shape.
func buildConfirmation(toolName string, res agent.Result) string {
	switch toolName {
	case "list_items":
		return renderItemList(res.Items)
	case "delete_item":
		return fmt.Sprintf("Archived %q.", itemTitle(res.Item))
	case "bulk_resolve":
		return renderResolutions(res.Resolutions)
	case "pause":
		return renderPauseConfirmation(res.Item, res.PausedUntil)
	case "create_goal", "update_goal":
		return renderGoalConfirmation(toolName, res.Goal, res.GoalProgress)
	case "list_goals":
		return renderGoalList(res.Goals)
	case "log_goal_progress":
		return renderGoalProgressLogged(res.Goal, res.GoalUpdate, res.GoalProgress)
	default: // create_item, update_item
		return renderNextOccurrences(res.Item, res.NextOccurrences, res.Inferred)
	}
}

// renderResolutions confirms a batch in the shape US-4.2 asks for: counts and
// names, not three separate confirmations, because the reply to "did stretching,
// vitamins and the walk" should read like an answer rather than a receipt.
//
// Rows are grouped by resulting status rather than listed in order. "Marked
// stretching and vitamins done, the walk skipped" is one sentence; the same
// three facts in row order are three clauses that repeat the verb.
//
// A row that applied nothing is named separately and honestly. Applied false is
// the idempotency table's second row - it was already in that state - and
// saying "already done" is both true and more useful than silently claiming
// credit for a write that did not happen.
func renderResolutions(rows []agent.Resolved) string {
	if len(rows) == 0 {
		return "Nothing to record."
	}

	byStatus := map[string][]string{}
	order := []string{}
	var alreadyDone int

	for _, r := range rows {
		if !r.Applied {
			alreadyDone++
			continue
		}
		name := r.ItemTitle
		if name == "" {
			// The item could not be read. Counting is the honest degradation:
			// naming an occurrence id back to a person is worse than not
			// naming anything.
			name = "one item"
		}
		if _, seen := byStatus[r.Status]; !seen {
			order = append(order, r.Status)
		}
		byStatus[r.Status] = append(byStatus[r.Status], name)
	}

	var parts []string
	for _, status := range order {
		parts = append(parts, fmt.Sprintf("%s %s", joinAnd(byStatus[status]), resolutionVerb(status)))
	}

	switch {
	case len(parts) == 0:
		return fmt.Sprintf("Already recorded - nothing changed (%d).", alreadyDone)
	case alreadyDone == 0:
		return "Got it - " + joinClauses(parts) + "."
	default:
		return fmt.Sprintf("Got it - %s. %d was already recorded.", joinClauses(parts), alreadyDone)
	}
}

// resolutionVerb is how a status reads at the end of a clause. missed has no
// case because the agent never writes one: a miss is what the reconciler
// concludes, not what a person reports (R2, behaviouralRules) - but it is
// handled rather than dropped, since bulk_resolve does accept it.
func resolutionVerb(status string) string {
	switch status {
	case string(domain.StatusCompleted):
		return "done"
	case string(domain.StatusSkipped):
		return "skipped"
	case string(domain.StatusMissed):
		return "recorded as missed"
	default:
		return "recorded as " + status
	}
}

// renderPauseConfirmation confirms a pause the user just set. Item is nil for a
// global one, which is why this cannot go through renderNextOccurrences: a
// global pause has no item to name and no next occurrence to show, because the
// point of it is that there are none.
//
// Named at length to keep it distinct from Ladder.renderPause in prompt.go,
// which renders the standing pause state into the prompt rather than confirming
// a change to it.
func renderPauseConfirmation(item *domain.Item, until *string) string {
	scope := "Everything"
	if item != nil {
		scope = fmt.Sprintf("%q", item.Title)
	}
	if until == nil {
		return fmt.Sprintf("%s is running normally again.", scope)
	}
	return fmt.Sprintf("%s is paused until %s. Nothing will fire or be checked up on until then.", scope, *until)
}

// joinAnd is joinOxford's "and" twin - a list of things that were all done,
// rather than a list of things being asked about.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

// joinClauses joins the per-status clauses with commas only. "stretching and
// vitamins done, the walk skipped" - a second "and" between the clauses would
// collide with the one inside the first.
func joinClauses(parts []string) string {
	return strings.Join(parts, ", ")
}

func itemTitle(item *domain.Item) string {
	if item == nil {
		return ""
	}
	return item.Title
}

func renderItemList(items []domain.Item) string {
	if len(items) == 0 {
		return "You have no matching items."
	}
	titles := make([]string, len(items))
	for i, it := range items {
		titles[i] = fmt.Sprintf("%q", it.Title)
	}
	return fmt.Sprintf("You have %d item(s): %s.", len(items), strings.Join(titles, ", "))
}

// renderNextOccurrences states the write in plain language, followed by
// every inferred parameter (A5: "a default applied silently is a wrong
// schedule nobody has a reason to look at") and the next concrete
// timestamps.
func renderNextOccurrences(item *domain.Item, occs []agent.Occurrence, inferred []schedule.Inference) string {
	name := itemTitle(item)
	clause := ""
	if desc := schedule.Describe(inferred); desc != "" {
		clause = fmt.Sprintf(" I assumed %s.", desc)
	}
	if len(occs) == 0 {
		return fmt.Sprintf("Done - %q is set.%s No upcoming occurrences to show yet.", name, clause)
	}
	return fmt.Sprintf("Done - %q is set.%s Next: %s.", name, clause, formatTimestamps(occs))
}

func formatTimestamps(occs []agent.Occurrence) string {
	texts := make([]string, len(occs))
	for i, o := range occs {
		texts[i] = o.StartsAt
	}
	return strings.Join(texts, ", ")
}

// renderGoalConfirmation confirms a create_goal or update_goal write in plain
// language, naming the shape and the current progress - which for an item-linked
// goal is already whatever its chains say, so "0 of 4 so far" is honest on the
// first turn rather than a placeholder.
func renderGoalConfirmation(toolName string, goal *domain.Goal, prog *store.GoalProgress) string {
	if goal == nil {
		return "Goal saved."
	}
	verb := "Tracking"
	if toolName == "update_goal" {
		verb = "Updated"
		if goal.Status == domain.GoalAbandoned {
			return fmt.Sprintf("Dropped %q.", goal.Title)
		}
	}
	period := goalPeriodPhrase(*goal)
	if prog == nil {
		return fmt.Sprintf("%s %q %s.", verb, goal.Title, period)
	}
	return fmt.Sprintf("%s %q %s. %s.", verb, goal.Title, period, goalProgressPhrase(*prog))
}

// renderGoalProgressLogged confirms a log_goal_progress append. It states the
// new current progress, which is the row just written - the trail is the data,
// so there is nothing else it could be.
func renderGoalProgressLogged(goal *domain.Goal, upd *domain.GoalUpdate, prog *store.GoalProgress) string {
	name := "the goal"
	if goal != nil {
		name = fmt.Sprintf("%q", goal.Title)
	}
	if prog != nil && !prog.ItemLinked {
		return fmt.Sprintf("Logged. %s is %s.", name, goalProgressPhrase(*prog))
	}
	if upd != nil && upd.Note != nil && *upd.Note != "" {
		return fmt.Sprintf("Logged against %s: %q.", name, *upd.Note)
	}
	return fmt.Sprintf("Logged against %s.", name)
}

// renderGoalList is list_goals' reply: a count and each goal's progress in one
// line, the same numbers context injection shows.
func renderGoalList(goals []store.GoalProgress) string {
	if len(goals) == 0 {
		return "You have no goals set."
	}
	parts := make([]string, len(goals))
	for i, p := range goals {
		parts[i] = fmt.Sprintf("%q (%s)", p.Goal.Title, goalProgressPhrase(p))
	}
	return fmt.Sprintf("You have %d goal(s): %s.", len(goals), strings.Join(parts, "; "))
}

// goalPeriodPhrase reads a goal's window back the way a person would say it.
func goalPeriodPhrase(g domain.Goal) string {
	switch g.PeriodKind {
	case domain.GoalPeriodDay:
		return "for " + g.PeriodStart
	case domain.GoalPeriodWeek:
		return fmt.Sprintf("for the week of %s", g.PeriodStart)
	case domain.GoalPeriodMonth:
		return fmt.Sprintf("for %s", g.PeriodStart[:7])
	default:
		return fmt.Sprintf("from %s to %s", g.PeriodStart, g.PeriodEnd)
	}
}

// goalProgressPhrase renders store.GoalProgress for a confirmation.
func goalProgressPhrase(p store.GoalProgress) string {
	if p.ItemLinked {
		return fmt.Sprintf("%d of %d so far", p.Completed, p.Target)
	}
	if !p.HasUpdate {
		return "no progress logged yet"
	}
	switch {
	case p.LatestPct != nil && p.LatestNote != nil && *p.LatestNote != "":
		return fmt.Sprintf("at %d%% (%q)", *p.LatestPct, *p.LatestNote)
	case p.LatestPct != nil:
		return fmt.Sprintf("at %d%%", *p.LatestPct)
	case p.LatestNote != nil:
		return fmt.Sprintf("last noted %q", *p.LatestNote)
	default:
		return "in progress"
	}
}

// buildApology is the terminal reply when the last failure was
// model.Complete()-level - the provider, not the user, is broken
// (docs/03-architecture.md's failure-mode table: "inbound messages get an
// explicit apology rather than silence"). Distinct from buildRephrase, which
// is about a request the user can fix.
func buildApology() string {
	return "Sorry, I couldn't reach the model provider just now, so nothing was changed. Reminders already scheduled will still fire - please try again in a bit."
}

// buildRephrase is the terminal reply when the last failure was tool-call
// side - L3's "ask the user to rephrase," after the ladder is exhausted.
func buildRephrase() string {
	return "I couldn't turn that into a valid change, so I've written nothing. Could you rephrase it?"
}
