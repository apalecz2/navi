package domain

import (
	"errors"
	"fmt"
)

// Status is an occurrence's position in its lifecycle. The valid set depends on
// the parent item's kind: reminders complete, events occur (D-017).
//
// There is deliberately no CHECK constraint on occurrences.status in the schema.
// The valid set is kind-dependent and the transitions between values are not
// expressible in a column constraint, so putting half the rule in the database
// would mean two places to keep in agreement. This file is the whole rule.
type Status string

const (
	// Reminders.
	StatusPending   Status = "pending"
	StatusNotified  Status = "notified"
	StatusCompleted Status = "completed"
	StatusSkipped   Status = "skipped"
	StatusSnoozed   Status = "snoozed"
	StatusMissed    Status = "missed"

	// Events, unused until P7.
	StatusOccurred  Status = "occurred"
	StatusCancelled Status = "cancelled"
)

// Outcome is what a caller should do about a requested transition. It exists so
// that idempotency is decided here and not by each surface: the notification
// buttons, the web app, and the agent all ask the same question and act on the
// same three answers (invariant 4).
//
// The HTTP layer's whole responsibility is the mapping, which carries no
// judgement of its own:
//
//	OutcomeApplied -> perform the write, 200 with the new state
//	OutcomeNoop    -> write nothing, 200 with the current state
//	OutcomeIllegal -> 409 with the current state
type Outcome uint8

const (
	// OutcomeApplied means the transition is legal and has not happened yet.
	OutcomeApplied Outcome = iota

	// OutcomeNoop means the occurrence is already in the requested terminal
	// state. A double-tapped notification button and a retried mobile request
	// both land here.
	OutcomeNoop

	// OutcomeIllegal means the transition is not permitted from the current
	// state. The accompanying error is a *TransitionError.
	OutcomeIllegal
)

// String renders an outcome for logs.
func (o Outcome) String() string {
	switch o {
	case OutcomeApplied:
		return "applied"
	case OutcomeNoop:
		return "noop"
	case OutcomeIllegal:
		return "illegal"
	default:
		return fmt.Sprintf("outcome(%d)", uint8(o))
	}
}

// reminderTransitions is the transition table from docs/04-data-model.md, as
// data rather than as a switch, so the legal edges can be read in one screen and
// compared against the document.
//
// A state absent from this map has no outgoing edges, which is what makes the
// terminal states terminal: everything about them falls out of the table plus
// the same-terminal rule in Transition.
var reminderTransitions = map[Status][]Status{
	// The scheduler notifies; the agent and the web app can resolve early
	// (R3), which cancels the pending notification. The reconciler applies
	// missed to silent items that were never notified.
	StatusPending: {StatusNotified, StatusCompleted, StatusSkipped, StatusMissed},

	// Any surface resolves a notified occurrence. Snooze is legal here and
	// carries a precondition the table cannot express — see CheckSnoozeCap.
	StatusNotified: {StatusCompleted, StatusSkipped, StatusSnoozed, StatusMissed},
}

// eventTransitions is the event set (P7). Same table, same machine, different
// valid values — which is the entire reason occurrences are generic over
// items.kind from day one (invariant 6, D-017).
var eventTransitions = map[Status][]Status{
	StatusPending: {StatusOccurred, StatusCancelled},
}

// transitionsFor returns the table for a kind.
func transitionsFor(k Kind) (map[Status][]Status, error) {
	switch k {
	case KindReminder:
		return reminderTransitions, nil
	case KindEvent:
		return eventTransitions, nil
	default:
		return nil, fmt.Errorf("domain: unknown kind %q", k)
	}
}

// IsTerminal reports whether a status ends the row's lifecycle for its kind.
//
// snoozed is terminal. The row is finished and keeps its true timestamp; the
// chain continues in the child occurrence (D-010). Resolving the chain therefore
// targets the live child, and completing a snoozed parent is rejected rather
// than allowed to write a second terminal state into one chain.
func (s Status) IsTerminal(k Kind) bool {
	table, err := transitionsFor(k)
	if err != nil {
		return false
	}
	_, hasOutgoing := table[s]
	return !hasOutgoing && s.valid(k)
}

// valid reports whether a status is a member of the kind's set at all.
func (s Status) valid(k Kind) bool {
	switch k {
	case KindReminder:
		switch s {
		case StatusPending, StatusNotified, StatusCompleted,
			StatusSkipped, StatusSnoozed, StatusMissed:
			return true
		}
	case KindEvent:
		switch s {
		case StatusPending, StatusOccurred, StatusCancelled:
			return true
		}
	}
	return false
}

// TransitionError describes a rejected transition. It is a typed error because
// a caller needs the fields: the HTTP layer reports From as the current state
// alongside its 409, and Message is copied verbatim into the message field of
// the error body (docs/07-api-spec.md), which is the same string the agent's
// escalation ladder feeds back to a model. That is the one place the wording
// has to name the rule and the offending values rather than say "invalid".
type TransitionError struct {
	Kind Kind
	From Status
	To   Status

	// Message is the caller-facing explanation, lowercase and specific.
	Message string
}

func (e *TransitionError) Error() string { return e.Message }

// ErrSnoozeCapReached is returned when a chain has been snoozed as many times
// as the item allows (R8). It is a precondition failure on an otherwise legal
// transition, so it is a sentinel rather than a missing edge in the table.
var ErrSnoozeCapReached = errors.New("domain: snooze cap reached")

// Transition reports what should happen when an occurrence of the given kind
// moves from one status to another. It is a pure function over the table above
// and touches no database — the caller performs the write when the outcome says
// to, inside whatever transaction it already holds.
//
// The two idempotency rules are the last two rows of the transition table in
// docs/04-data-model.md, and they are the whole reason this returns three
// outcomes instead of an error:
//
//   - already in the requested terminal state: OutcomeNoop
//   - in a different terminal state: OutcomeIllegal
func Transition(kind Kind, from, to Status) (Outcome, error) {
	table, err := transitionsFor(kind)
	if err != nil {
		return OutcomeIllegal, &TransitionError{
			Kind: kind, From: from, To: to,
			Message: fmt.Sprintf("unknown item kind %q", kind),
		}
	}

	if !from.valid(kind) {
		return OutcomeIllegal, &TransitionError{
			Kind: kind, From: from, To: to,
			Message: fmt.Sprintf("%q is not a status a %s occurrence can hold", from, kind),
		}
	}
	if !to.valid(kind) {
		return OutcomeIllegal, &TransitionError{
			Kind: kind, From: from, To: to,
			Message: fmt.Sprintf("%q is not a status a %s occurrence can hold", to, kind),
		}
	}

	// Same terminal state, from any surface: nothing to write, and not an
	// error. A double-tapped button lands here.
	if from == to && from.IsTerminal(kind) {
		return OutcomeNoop, nil
	}

	for _, allowed := range table[from] {
		if allowed == to {
			return OutcomeApplied, nil
		}
	}

	// A different terminal state is the case worth naming precisely, because it
	// is the one a user can cause and the one whose message they will read.
	if from.IsTerminal(kind) {
		return OutcomeIllegal, &TransitionError{
			Kind: kind, From: from, To: to,
			Message: fmt.Sprintf("occurrence is already %s and cannot become %s", from, to),
		}
	}

	return OutcomeIllegal, &TransitionError{
		Kind: kind, From: from, To: to,
		Message: fmt.Sprintf("a %s occurrence cannot go from %s to %s", kind, from, to),
	}
}

// CheckSnoozeCap reports whether another snooze is permitted. depth is the
// current occurrence's snooze_depth and cap is items.snooze_cap.
//
// This is separate from Transition because notified -> snoozed is a legal edge
// that carries a precondition the table cannot hold. Past the cap the chain
// resolves as missed (R8), so a caller that gets ErrSnoozeCapReached transitions
// to missed rather than reporting a failure.
func CheckSnoozeCap(depth, snoozeCap int) error {
	if depth >= snoozeCap {
		return fmt.Errorf("%w: depth %d of %d", ErrSnoozeCapReached, depth, snoozeCap)
	}
	return nil
}
