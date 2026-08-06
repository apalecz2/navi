package domain

import "fmt"

// ValidationError is a rejected value, described well enough to act on.
//
// It is a typed error for the same reason TransitionError is: a caller needs
// the fields. The HTTP layer renders all three into the error body that
// docs/07-api-spec.md specifies —
//
//	{"error": "validation_failed",
//	 "message": "min_gap_hours 8 cannot be satisfied with count 5 over period 'day'",
//	 "field": "schedule.min_gap_hours"}
//
// — and that message is fed back to a model verbatim on the same-tier retry of
// the escalation ladder in docs/06-agent-spec.md. This is why every message
// names the rule and the offending values instead of saying "invalid
// schedule": a generic string removes the retry rung silently, leaving a ladder
// that still runs and never fixes anything.
//
// It lives in domain rather than in internal/schedule because three unrelated
// places produce one: the schedule validator, the store's "does this item_id
// resolve" check, and P1's tool-argument decoding. A leaf is reachable from all
// three; internal/schedule is not.
type ValidationError struct {
	// Rule is the stable identifier of the check that failed, matching a row of
	// the table in docs/05-schedule-spec.md#validation. It is for logs and
	// metrics, never shown to a user — Message is the human-facing half.
	Rule string

	// Field is the dotted path to the offending value, rooted at the request
	// body: "schedule.min_gap_hours", "schedule.window[0]", "tz".
	Field string

	// Message is the caller-facing explanation. Lowercase, no trailing
	// punctuation, and it names the rule and the values.
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// Invalid builds a ValidationError. The message is formatted here so that
// every call site reads as one line and none of them is tempted to build the
// struct with an empty Rule.
func Invalid(rule, field, format string, args ...any) *ValidationError {
	return &ValidationError{
		Rule:    rule,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	}
}
