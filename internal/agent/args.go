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
	GracePeriodMinutes *int              `json:"grace_period_minutes,omitempty" jsonschema:"minimum=1"`
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
	Priority *int `json:"priority,omitempty" jsonschema:"minimum=1,maximum=5"`

	// minimum=1, matching CreateItemArgs: zero is the value that would let the
	// tick that asks about an occurrence also mark it missed, which is K6
	// defeated by a config value. domain.GraceDeadline treats a non-positive
	// stored value as absent, so this is the front door rather than the only
	// defence.
	GracePeriodMinutes *int    `json:"grace_period_minutes,omitempty" jsonschema:"minimum=1"`
	ReconcileAt        *string `json:"reconcile_at,omitempty"`
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

// The four goal tool argument structs (P3.5, docs/06-agent-spec.md#tool-catalog,
// docs/11-goals-spec.md). Same three validation layers and same registry as the
// P1 tools; nothing in the escalation ladder changes to admit them.

// CreateGoalArgs is create_goal's arguments.
//
// period_start defaults to today and period_end is computed from period_kind
// when omitted; for period_kind=custom, period_end is required. item_id set
// makes it item-linked and then target_count is required and must be >= 1;
// item_id absent makes it freestanding and target_count must be absent. That
// XOR is Layer 2, not a schema constraint, because JSON Schema cannot express
// "required depending on another field" in a form every model reads, and a
// rejection naming the rule helps the ladder more than a keyword error.
type CreateGoalArgs struct {
	Title       string  `json:"title" jsonschema:"required"`
	PeriodKind  string  `json:"period_kind" jsonschema:"required,enum=day,enum=week,enum=month,enum=custom"`
	PeriodStart *string `json:"period_start,omitempty"`
	PeriodEnd   *string `json:"period_end,omitempty"`
	ItemID      *string `json:"item_id,omitempty"`
	TargetCount *int    `json:"target_count,omitempty" jsonschema:"minimum=1"`
}

// GoalChanges is update_goal's "only fields being changed" - not spelled out in
// 06-agent-spec.md, designed here as the all-optional mirror ItemChanges is for
// update_item. period_kind is deliberately absent: changing the shape of the
// window is a new goal, not an edit. status carries only abandoned, the one
// terminal transition a user drives; met and missed are the sweeper's to write.
type GoalChanges struct {
	Title       *string `json:"title,omitempty"`
	PeriodStart *string `json:"period_start,omitempty"`
	PeriodEnd   *string `json:"period_end,omitempty"`
	TargetCount *int    `json:"target_count,omitempty" jsonschema:"minimum=1"`
	Status      *string `json:"status,omitempty" jsonschema:"enum=abandoned"`
}

// UpdateGoalArgs is update_goal's arguments.
type UpdateGoalArgs struct {
	GoalID  string      `json:"goal_id" jsonschema:"required"`
	Changes GoalChanges `json:"changes" jsonschema:"required"`
}

// ListGoalsArgs is list_goals' arguments.
type ListGoalsArgs struct {
	Filter string `json:"filter,omitempty" jsonschema:"enum=active,enum=all,default=active"`
}

// LogGoalProgressArgs is log_goal_progress's arguments - a conversational turn,
// not a resolution. At least one of progress_pct or note is required (Layer 2):
// a call with neither writes nothing and is rejected the same shape as any
// other under-specified tool call.
type LogGoalProgressArgs struct {
	GoalID      string  `json:"goal_id" jsonschema:"required"`
	ProgressPct *int    `json:"progress_pct,omitempty" jsonschema:"minimum=0,maximum=100"`
	Note        *string `json:"note,omitempty"`
}

// RequestEscalationArgs is request_escalation's arguments - the escalation
// ladder's own trigger (docs/06-agent-spec.md#escalation-ladder, L4), not
// one of the four P1 CRUD tools. It writes nothing; see escalation.go.
type RequestEscalationArgs struct {
	Reason string `json:"reason" jsonschema:"required"`
}
