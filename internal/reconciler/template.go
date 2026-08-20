package reconciler

import (
	"strings"

	"github.com/aidenpaleczny/navi/internal/store"
)

// composeTemplate renders the check-in as the plain templated list D-009
// requires as its fallback, in the shape 06-agent-spec's Reconciler composer
// illustrates:
//
//	Haven't heard about stretching, vitamins, or the evening walk.
//	Which of those got done?
//
// Three rules from that section are structural here rather than entrusted to
// the model that composes the ordinary path (compose.go). That is the point of
// a fallback: it cannot get the tone wrong, so it is safe to reach for at the
// moment there is no way to check.
//
//   - One message, never one per item (K4). The whole list is one sentence.
//   - Never accusatory. It is a question, and it says what was not heard rather
//     than what was not done. Nothing counts the items, names a streak, or
//     mentions how long anything has been outstanding — persona.md's G8 binds
//     this text the same way it binds reminder copy (Q-16).
//   - Titles are named, not ids, because the reply is prose that has to name
//     them back.
//
// Rows are deduplicated by item while every row keeps its own reconciled_at:
// two unresolved occurrences of one reminder are one thing the user did or did
// not do, and listing "vitamins, vitamins" would be the audit this is not.
// Order follows the caller's, which is starts_at — earliest first, so the list
// reads in the order the day happened. The composer is handed the same list
// from the same helper, so falling back mid-evening does not change which items
// are named or in what order.
func composeTemplate(outstanding []store.Unreconciled) string {
	titles := dedupeTitles(outstanding)

	if len(titles) == 0 {
		return ""
	}
	if len(titles) == 1 {
		// "Which of those" needs a those. One item gets the singular question
		// rather than a list of one.
		return "Haven't heard about " + titles[0] + ". Did that get done?"
	}
	return "Haven't heard about " + joinOxford(titles) + ".\nWhich of those got done?"
}

// joinOxford renders a list the way a person writes one: "a and b" for two,
// "a, b, or c" for more. The serial comma is there because the alternative
// reads as two items when the last title contains an "and" of its own, which
// titles like "vitamins and the walk" routinely do.
func joinOxford(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
	}
}
