package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aidenpaleczny/navi/internal/model"
)

// Handler is what one tool call runs against. raw is the model's (or, this
// session, naviseed's) unparsed arguments; every handler owns its own
// Layer 1/2/3 pipeline, because the three layers are typed per argument
// struct and cannot be generalized behind one reflection-only walk without
// losing the per-field error messages 07-api-spec.md requires.
type Handler func(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error)

// registration is a tool's name, its model-facing description, its handler,
// and the zero value of its argument struct (for schema generation). It is
// the one place a new tool is added - the whole surface bulk_resolve
// (session 15) touched to register itself, and the whole surface snooze,
// pause, get_stats and P3's later tools will. Nothing in decode.go,
// schema.go, or Call/Catalog's bodies needs to change for a new tool to show
// up in both.
type registration struct {
	name        string
	description string
	handler     Handler
	args        any
}

var registrations = []registration{
	{"list_items", "List items, optionally filtered by active, all, or paused.", handleListItems, ListItemsArgs{}},
	{"create_item", "Create a new reminder or event with a schedule.", handleCreateItem, CreateItemArgs{}},
	{"update_item", "Change an existing item's fields or schedule, at a chosen scope.", handleUpdateItem, UpdateItemArgs{}},
	{"delete_item", "Archive an item and remove its pending occurrences. Requires confirmed=true.", handleDeleteItem, DeleteItemArgs{}},
	{"bulk_resolve", "Record outcomes for one or more occurrences in a single atomic write. Use this for any message reporting a completion, a skip, or several at once — including a single one, and including something already done earlier today.", handleBulkResolve, BulkResolveArgs{}},
	{"pause", "Suspend everything, or one item, until a date. Use this whenever the user says they are away or unavailable, instead of skipping each occurrence. An empty until resumes.", handlePause, PauseArgs{}},
	{"create_goal", "Set a target over a period. Link it to an existing item with item_id plus a target_count (\"gym four times this week\"), or leave both off for a freestanding goal tracked by conversation (\"ship the report by Friday\"). period_kind is day, week, month, or custom; for custom, give period_end.", handleCreateGoal, CreateGoalArgs{}},
	{"update_goal", "Change a goal's title, period_end, or target_count, or set status to abandoned to drop it. Rejected on a goal that has already ended (met, missed, or abandoned).", handleUpdateGoal, UpdateGoalArgs{}},
	{"list_goals", "List goals, active by default or all.", handleListGoals, ListGoalsArgs{}},
	{"log_goal_progress", "Record progress on a freestanding goal from the conversation - a percent, a note, or both. Not for occurrences: a goal is not resolved and does not go through the status machine. Rejected on a goal that has already ended.", handleLogGoalProgress, LogGoalProgressArgs{}},
	{"request_escalation", "Terminate this turn and retry at the next tier. Call when the request is ambiguous, spans multiple items in a way that is hard to disentangle, or references something unresolvable. Writes nothing.", handleRequestEscalation, RequestEscalationArgs{}},
}

// Call looks a tool up by name and runs it. It is the whole of what
// cmd/naviseed drives this session and what a later session's escalation
// ladder drives on top of a real model.
func (t *Tools) Call(ctx context.Context, name string, raw json.RawMessage) (Result, error) {
	for _, r := range registrations {
		if r.name == name {
			return r.handler(ctx, t, raw)
		}
	}
	return Result{}, fmt.Errorf("agent: unknown tool %q", name)
}

// Catalog renders every registration's schema, generated from the same
// argument struct Call decodes into - model.Tool.Parameters is raw JSON
// already, so this is the whole bridge to internal/model's Tool type.
// Nothing consumes it this session; it is generated and printed so it can be
// read.
func (t *Tools) Catalog() []model.Tool {
	out := make([]model.Tool, len(registrations))
	for i, r := range registrations {
		out[i] = model.Tool{Name: r.name, Description: r.description, Parameters: schemaFor(r.args)}
	}
	return out
}
