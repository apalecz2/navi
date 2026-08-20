package agent

import (
	"encoding/json"

	"github.com/aidenpaleczny/navi/internal/schedule"
)

// The four P1 tool argument structs, from docs/06-agent-spec.md#tool-catalog.
// Optional fields are pointers rather than zero values: 0 and omitted are the
// same value for a plain int, and priority has a non-zero default, so a
// caller that omits the field would otherwise silently get priority zero and
// fail validation. This is also why CreateItemArgs.Priority is *int here
// rather than the unpointered int the doc's own code sample shows - the
// doc's prose says the pointer is the point, and the sample predates it (see
// the session plan for the full note).
//
// jsonschema tags drive two independent readers off one string: schema.go's
// generator, and decode.go's validateArgs. That is the whole reason to avoid
// a second, differently-spelled tag namespace for validation.

// ListItemsArgs is list_items' arguments.
type ListItemsArgs struct {
	Filter string `json:"filter,omitempty" jsonschema:"enum=active,enum=all,enum=paused,default=active"`
}

// CreateItemArgs is create_item's arguments.
type CreateItemArgs struct {
	Title              string            `json:"title" jsonschema:"required"`
	Schedule           schedule.Schedule `json:"schedule" jsonschema:"required"`
	Notes              *string           `json:"notes,omitempty"`
	Kind               string            `json:"kind,omitempty" jsonschema:"enum=reminder,enum=event,default=reminder"`
	TZ                 *string           `json:"tz,omitempty"`
	TZMode             string            `json:"tz_mode,omitempty" jsonschema:"enum=fixed,enum=floating,default=floating"`
	NotifyPolicy       string            `json:"notify_policy,omitempty" jsonschema:"enum=at_time,enum=silent,enum=digest,default=at_time"`
	Priority           *int              `json:"priority,omitempty" jsonschema:"minimum=1,maximum=5,default=3"`
	GracePeriodMinutes *int              `json:"grace_period_minutes,omitempty"`
	ReconcileAt        *string           `json:"reconcile_at,omitempty"`
}

// Edit scopes, docs/05-schedule-spec.md#edit-scope.
const (
	ScopeFutureAll = "future_all"
	ScopeFromDate  = "from_date"
	ScopeSingle    = "single"
)

// UpdateItemArgs is update_item's arguments.
type UpdateItemArgs struct {
	ItemID       string      `json:"item_id" jsonschema:"required"`
	Scope        string      `json:"scope,omitempty" jsonschema:"enum=future_all,enum=from_date,enum=single,default=future_all"`
	FromDate     *string     `json:"from_date,omitempty"`
	OccurrenceID *string     `json:"occurrence_id,omitempty"`
	Changes      ItemChanges `json:"changes" jsonschema:"required"`
}

// ItemChanges is "only fields being changed" - not spelled out in
// 06-agent-spec.md, designed here as an all-optional mirror of
// CreateItemArgs, reusing schedule.Schedule directly for the nested
// schedule (its own doc comment names exactly this reuse).
type ItemChanges struct {
	Title              *string            `json:"title,omitempty"`
	Notes              *string            `json:"notes,omitempty"`
	Schedule           *schedule.Schedule `json:"schedule,omitempty"`
	Kind               *string            `json:"kind,omitempty" jsonschema:"enum=reminder,enum=event"`
	TZ                 *string            `json:"tz,omitempty"`
	TZMode             *string            `json:"tz_mode,omitempty" jsonschema:"enum=fixed,enum=floating"`
	NotifyPolicy       *string            `json:"notify_policy,omitempty" jsonschema:"enum=at_time,enum=silent,enum=digest"`
	Priority           *int               `json:"priority,omitempty" jsonschema:"minimum=1,maximum=5"`
	GracePeriodMinutes *int               `json:"grace_period_minutes,omitempty"`
	ReconcileAt        *string            `json:"reconcile_at,omitempty"`
	Attrs              json.RawMessage    `json:"attrs,omitempty"`
}

// DeleteItemArgs is delete_item's arguments.
type DeleteItemArgs struct {
	ItemID    string `json:"item_id" jsonschema:"required"`
	Confirmed bool   `json:"confirmed"`
}

// BulkResolveArgs is bulk_resolve's arguments, from
// docs/06-agent-spec.md#tool-catalog.
//
// A list rather than a single occurrence id, and deliberately so: six
// sequential single calls are six chances to fail, six validation passes, and a
// partial application when the fourth is wrong. One tool taking a list gives one
// transaction and an all-or-nothing outcome, which is what "did everything
// except the walk" actually needs. A batch of one is the degenerate case and is
// what "did my stretching already" produces (R3).
type BulkResolveArgs struct {
	Resolutions []ResolutionArg `json:"resolutions" jsonschema:"required,minItems=1"`
}

// ResolutionArg is one occurrence's outcome inside a bulk_resolve call.
//
// The enum is the same three statuses domain.ParseResolvableStatus decodes, and
// it is enforced at Layer 1 by decode.go's walk into slice elements — which is
// the reason that walk exists.
type ResolutionArg struct {
	OccurrenceID string  `json:"occurrence_id" jsonschema:"required"`
	Status       string  `json:"status" jsonschema:"required,enum=completed,enum=skipped,enum=missed"`
	Note         *string `json:"note,omitempty"`
}

// PauseArgs is pause's arguments, verbatim from
// docs/06-agent-spec.md#tool-catalog.
//
// until is an ISO date rather than an instant, because "I'm away until Monday"
// is a date and resolving it to a wall clock is the server's job, not the
// model's. An empty until lifts the pause, which is how "I'm back" is expressed
// without a second tool.
//
// item_id is required for scope=item and must be absent for scope=global -
// enforced at Layer 2, not by the schema, because JSON Schema cannot express
// "required depending on another field" in a form every model reads reliably,
// and a rejection naming the rule is more useful to the escalation ladder than
// a schema error naming a keyword.
type PauseArgs struct {
	Scope  string  `json:"scope" jsonschema:"required,enum=global,enum=item"`
	Until  string  `json:"until"`
	ItemID *string `json:"item_id,omitempty"`
}

// RequestEscalationArgs is request_escalation's arguments - the escalation
// ladder's own trigger (docs/06-agent-spec.md#escalation-ladder, L4), not
// one of the four P1 CRUD tools. It writes nothing; see escalation.go.
type RequestEscalationArgs struct {
	Reason string `json:"reason" jsonschema:"required"`
}
