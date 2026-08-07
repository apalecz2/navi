package domain

import (
	"fmt"
	"strings"
	"time"
)

// ResolutionSource records which surface resolved an occurrence. After a month
// it says whether resolutions actually come from notifications, the web app, or
// a message, which is what should decide where effort goes next.
type ResolutionSource string

const (
	ResolvedByNotification ResolutionSource = "notification"
	ResolvedByWeb          ResolutionSource = "web"
	ResolvedByAgent        ResolutionSource = "agent"
	ResolvedBySweeper      ResolutionSource = "sweeper"
)

// Generation pass values for message_text, from the copywriter's two-pass
// schedule. Stored so a refreshed message is distinguishable from a safety-net
// one without inspecting the text.
const (
	GenerationPassNone   = 0
	GenerationPassSafety = 1
	GenerationPassFresh  = 2
)

// Occurrence is one materialized instance of an item: the row that
// notifications, resolution, the calendar, and the statistics all operate on.
//
// History is immutable (invariant 2). Only pending occurrences are ever deleted
// or rewritten, so editing a schedule never alters what already happened.
type Occurrence struct {
	ID     string
	ItemID string

	StartsAt time.Time
	EndsAt   *time.Time // nil for instants, which is every reminder

	Status Status

	// IsOverride marks a row the materializer must not touch. It is the entire
	// mechanism behind "skip tomorrow's" and behind snooze children: without it
	// a single-occurrence edit is silently undone by the next expansion run.
	IsOverride bool

	// ParentOccurrenceID and SnoozeDepth form the snooze chain. Snoozing writes
	// a child and marks the original snoozed; it never moves starts_at, because
	// that would destroy the record that something was due at 09:00 and got
	// pushed three times (D-010).
	ParentOccurrenceID *string
	SnoozeDepth        int

	NotifiedAt   *time.Time
	ReconciledAt *time.Time
	ResolvedAt   *time.Time

	ResolutionNote   *string
	ResolutionSource *ResolutionSource

	// MessageText is the copywriter's contribution, generated in advance and
	// stored. The scheduler falls back to the item title when it is nil, which
	// is what keeps the model out of the firing path (invariant 1).
	MessageText        *string
	MessageModel       *string
	MessageGeneratedAt *time.Time
	GenerationAttempts int
	GenerationPass     int

	CreatedAt time.Time
}

// NotificationBody is the text a surface shows for an occurrence: the
// copywriter's generated message when there is one, the plain item title when
// there is not (N3).
//
// It lives in this leaf rather than beside the scheduler because the fallback is
// a user-facing rule, not a fire-path detail — the reconciler's check-in, the
// day view, and the copywriter's own tone decisions all need the same answer,
// and each of them reaching it through a store row type would mean either
// importing one or reimplementing the rule. Same argument that puts the
// transition table here.
//
// An empty generated string counts as absent. A blank message is a generation
// that went wrong, and sending nothing at all is worse than sending the title.
func NotificationBody(messageText *string, title string) string {
	if messageText != nil && strings.TrimSpace(*messageText) != "" {
		return *messageText
	}
	return title
}

// NewOccurrence is what a caller supplies to materialize an instance. The store
// assigns the id and created_at.
type NewOccurrence struct {
	ItemID   string
	StartsAt time.Time
	EndsAt   *time.Time

	// Status defaults to pending. It is settable because a backfilled or
	// hand-inserted row is occasionally something else.
	Status *Status

	IsOverride         bool
	ParentOccurrenceID *string
	SnoozeDepth        *int

	MessageText *string
}

// WithDefaults returns a copy with every omitted field filled in, matching the
// DDL defaults.
func (n NewOccurrence) WithDefaults() NewOccurrence {
	if n.Status == nil {
		s := StatusPending
		n.Status = &s
	}
	if n.SnoozeDepth == nil {
		d := 0
		n.SnoozeDepth = &d
	}
	return n
}

// Validate checks the invariants the schema cannot express. The status set is
// kind-dependent, so it is checked against the parent item's kind rather than
// against a column constraint.
func (n NewOccurrence) Validate(kind Kind) error {
	if n.ItemID == "" {
		return fmt.Errorf("domain: item_id is required")
	}
	if n.StartsAt.IsZero() {
		return fmt.Errorf("domain: starts_at is required")
	}
	if n.EndsAt != nil && n.EndsAt.Before(n.StartsAt) {
		return fmt.Errorf("domain: ends_at %s is before starts_at %s",
			FormatTime(*n.EndsAt), FormatTime(n.StartsAt))
	}
	if n.Status != nil && !n.Status.valid(kind) {
		return fmt.Errorf("domain: %q is not a status a %s occurrence can hold", *n.Status, kind)
	}
	if n.SnoozeDepth != nil && *n.SnoozeDepth < 0 {
		return fmt.Errorf("domain: snooze_depth %d is negative", *n.SnoozeDepth)
	}
	// A snooze child is always an override, because the materializer would
	// otherwise delete it on the next run.
	if n.ParentOccurrenceID != nil && !n.IsOverride {
		return fmt.Errorf("domain: a child occurrence must set is_override")
	}
	return nil
}
