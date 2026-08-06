// Package domain holds the types this system reasons about and the rules that
// govern them: items, occurrences, and the status state machine.
//
// It is a leaf. It imports the standard library and a ULID generator, and
// nothing else — no SQL, no store, no HTTP. The dependency runs the other way:
// internal/store imports domain and returns these types, so the loops, the
// handlers, and the agent all reach the transition rules without reaching a
// database. That is what lets one state machine serve every surface (invariant
// 4) instead of each surface growing its own copy.
//
// Optional fields are pointers rather than zero values. For a plain int, 0 and
// omitted are the same value, and priority has a non-zero default; making
// "absent" representable is what lets defaults be applied in one place after
// decoding rather than guessed at each use.
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Kind distinguishes the two things an item can be. Reminders are built now;
// events are P7. The schema, the materializer, the scheduler, and the agent
// tools are generic over this column from day one, because they are shared
// between the two and retrofitting the split later means a parallel stack or a
// migration through live data (D-017, invariant 6).
type Kind string

const (
	KindReminder Kind = "reminder"
	KindEvent    Kind = "event"
)

// TZMode decides how an item's timezone is resolved at materialization. A fixed
// item is always in its own zone; a floating item follows the device, which is
// the kv key current_tz.
type TZMode string

const (
	TZModeFixed    TZMode = "fixed"
	TZModeFloating TZMode = "floating"
)

// NotifyPolicy decides whether an occurrence is notified at its time, resolved
// silently, or rolled into a digest. The scheduler claims only at_time rows.
type NotifyPolicy string

const (
	NotifyAtTime NotifyPolicy = "at_time"
	NotifySilent NotifyPolicy = "silent"
	NotifyDigest NotifyPolicy = "digest"
)

// Source records where an item came from. Everything is local until calendar
// sync exists; the column is here from the start so that adding sync is not a
// migration through live data (I5).
type Source string

const (
	SourceLocal  Source = "local"
	SourceGoogle Source = "google"
	SourceApple  Source = "apple"
)

// Item is the definition of a recurring thing. It never holds a timestamp for a
// specific day — that is what an Occurrence is for (D-003).
type Item struct {
	ID    string
	Kind  Kind
	Title string
	Notes *string

	// Schedule is the tagged union in docs/05-schedule-spec.md, held as raw
	// JSON here. Session 3 gives it a parsed type; the store does not change
	// when it does, because the column is TEXT either way.
	Schedule json.RawMessage

	// TZ is an IANA location name. TZMode decides whether it is authoritative
	// or a fallback for the device zone.
	TZ     string
	TZMode TZMode

	NotifyPolicy NotifyPolicy
	Priority     int

	// GracePeriodMinutes is how long after the start time the reconciler waits
	// before assigning missed. Nil means end of the local day.
	GracePeriodMinutes *int

	// ReconcileAt is a local HH:MM. Nil means the global default.
	ReconcileAt *string

	SnoozeCap int

	Active      bool
	PausedUntil *time.Time

	// ArchivedAt replaces deletion. Resolved occurrences reference the item and
	// the statistics need its title, so an item is archived and its pending
	// occurrences removed.
	ArchivedAt *time.Time

	// Attrs is kind-specific JSON: location, attendees, whatever events need.
	// It exists so that adding event fields later is not twelve nullable
	// columns that are always null for reminders (I4).
	Attrs json.RawMessage

	Source       Source
	ExternalID   *string
	ETag         *string
	LastSyncedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsPaused reports whether the item is paused at the given instant (I6).
func (i Item) IsPaused(at time.Time) bool {
	return i.PausedUntil != nil && i.PausedUntil.After(at)
}

// NewItem is what a caller supplies to create an item. The store assigns the
// id and the timestamps, so no caller invents either.
//
// The fields with non-zero defaults are pointers: priority defaults to 3 and
// snooze_cap to 3, and a plain int cannot distinguish "the caller wants 0" from
// "the caller said nothing".
type NewItem struct {
	Kind     Kind
	Title    string
	Notes    *string
	Schedule json.RawMessage
	TZ       string

	TZMode             *TZMode
	NotifyPolicy       *NotifyPolicy
	Priority           *int
	GracePeriodMinutes *int
	ReconcileAt        *string
	SnoozeCap          *int
	Attrs              json.RawMessage
}

// Defaults for the fields a caller may omit. They match the DDL, which is what
// keeps a row created through the store identical to one created by a bare
// INSERT.
const (
	DefaultPriority  = 3
	DefaultSnoozeCap = 3
)

// WithDefaults returns a copy with every omitted field filled in. It is applied
// in exactly one place — the store's create path — so a default lives here and
// in the DDL and nowhere else.
func (n NewItem) WithDefaults() NewItem {
	if n.Kind == "" {
		n.Kind = KindReminder
	}
	if n.TZMode == nil {
		m := TZModeFloating
		n.TZMode = &m
	}
	if n.NotifyPolicy == nil {
		p := NotifyAtTime
		n.NotifyPolicy = &p
	}
	if n.Priority == nil {
		p := DefaultPriority
		n.Priority = &p
	}
	if n.SnoozeCap == nil {
		c := DefaultSnoozeCap
		n.SnoozeCap = &c
	}
	if len(n.Attrs) == 0 {
		n.Attrs = json.RawMessage(`{}`)
	}
	return n
}

// Validate checks what the schema's CHECK constraints would catch, so the error
// names the field rather than surfacing a driver message about a constraint.
// Schedule contents are session 3's business and are only checked for being
// present and syntactically JSON.
func (n NewItem) Validate() error {
	switch n.Kind {
	case KindReminder, KindEvent:
	default:
		return fmt.Errorf("domain: kind %q is not reminder or event", n.Kind)
	}
	if n.Title == "" {
		return fmt.Errorf("domain: title is required")
	}
	if n.TZ == "" {
		return fmt.Errorf("domain: tz is required")
	}
	if _, err := time.LoadLocation(n.TZ); err != nil {
		return fmt.Errorf("domain: tz %q is not an IANA location: %w", n.TZ, err)
	}
	if len(n.Schedule) == 0 {
		return fmt.Errorf("domain: schedule is required")
	}
	if !json.Valid(n.Schedule) {
		return fmt.Errorf("domain: schedule is not valid JSON")
	}
	if len(n.Attrs) > 0 && !json.Valid(n.Attrs) {
		return fmt.Errorf("domain: attrs is not valid JSON")
	}
	if n.TZMode != nil {
		switch *n.TZMode {
		case TZModeFixed, TZModeFloating:
		default:
			return fmt.Errorf("domain: tz_mode %q is not fixed or floating", *n.TZMode)
		}
	}
	if n.NotifyPolicy != nil {
		switch *n.NotifyPolicy {
		case NotifyAtTime, NotifySilent, NotifyDigest:
		default:
			return fmt.Errorf("domain: notify_policy %q is not at_time, silent or digest", *n.NotifyPolicy)
		}
	}
	if n.Priority != nil && (*n.Priority < 1 || *n.Priority > 5) {
		return fmt.Errorf("domain: priority %d is outside 1..5", *n.Priority)
	}
	if n.SnoozeCap != nil && *n.SnoozeCap < 0 {
		return fmt.Errorf("domain: snooze_cap %d is negative", *n.SnoozeCap)
	}
	return nil
}
