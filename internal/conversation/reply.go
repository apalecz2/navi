package conversation

import (
	"fmt"
	"strings"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
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
	default: // create_item, update_item
		return renderNextOccurrences(res.Item, res.NextOccurrences, res.Inferred)
	}
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
