package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// The goal tool handlers (P3.5, docs/11-goals-spec.md). Each owns its own
// Layer 1/2/3 pipeline, the same as the item handlers in execute.go. Layer 2
// here is the goal validation table: period ordering and calendar alignment,
// the target/item XOR, a referenced item resolving live, and the terminal-goal
// guard on update_goal and log_goal_progress. All of it produces
// *domain.ValidationError, so a rejection reads like the schedule ones the
// escalation ladder already feeds back to a model.

// localZone is the person's clock - kv.current_tz, then the deployment default -
// resolved the same way resolveCreateZone and zoneFor resolve theirs, and the
// same one the reconciler check-in and pause use. A goal's period dates are
// local, so this is what turns "week" into concrete Monday-Sunday dates and
// what the period-end pass measures "today" against.
func (t *Tools) localZone(ctx context.Context) *time.Location {
	zones := schedule.Zones{Fallback: t.defaultTZ}
	if name, ok, err := t.store.CurrentTZ(ctx); err == nil && ok {
		if loc, err := schedule.LoadLocation(name); err == nil {
			zones.Device = loc
		}
	}
	return zones.Local()
}

func handleCreateGoal(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[CreateGoalArgs](raw)
	if err != nil {
		return Result{}, err
	}

	kind := domain.GoalPeriodKind(args.PeriodKind)
	loc := t.localZone(ctx)
	now := time.Now()

	// The target/item XOR. Item-linked needs a target of at least 1;
	// freestanding takes none.
	if err := validateGoalTargetXOR(args.ItemID, args.TargetCount); err != nil {
		return Result{}, err
	}

	// A referenced item must resolve to a live, non-archived row. LiveItem
	// returns exactly the *domain.ValidationError shape this rule wants, with
	// field "item_id".
	if args.ItemID != nil && *args.ItemID != "" {
		if _, err := t.store.LiveItem(ctx, *args.ItemID); err != nil {
			return Result{}, err
		}
	}

	start, end, err := resolveGoalPeriod(kind, args.PeriodStart, args.PeriodEnd, now, loc)
	if err != nil {
		return Result{}, err
	}
	if err := validateGoalPeriod(kind, start, end); err != nil {
		return Result{}, err
	}

	goal, err := t.store.CreateGoal(ctx, domain.NewGoal{
		Title:       args.Title,
		PeriodKind:  kind,
		PeriodStart: start,
		PeriodEnd:   end,
		ItemID:      args.ItemID,
		TargetCount: args.TargetCount,
	}, now)
	if err != nil {
		return Result{}, wrapValidation(err)
	}

	prog, err := t.store.GoalProgressFor(ctx, goal, loc)
	if err != nil {
		return Result{}, err
	}
	return Result{Goal: &goal, GoalProgress: &prog}, nil
}

func handleUpdateGoal(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[UpdateGoalArgs](raw)
	if err != nil {
		return Result{}, err
	}

	goal, err := t.store.GetGoal(ctx, args.GoalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, domain.Invalid("goal_exists", "goal_id", "goal %q does not exist", args.GoalID)
		}
		return Result{}, err
	}
	// The terminal-goal guard (docs/11-goals-spec.md#validation). A goal that
	// has ended is history; pushing its deadline was update_goal-shaped while it
	// was still active.
	if goal.Status.IsTerminal() {
		return Result{}, domain.Invalid("goal_terminal", "goal_id",
			"goal %q is %s and can no longer be updated", args.GoalID, goal.Status)
	}

	c := args.Changes

	// Re-run the XOR against the merged view: item_id cannot change (it is not
	// in GoalChanges), but target_count can, and clearing it on an item-linked
	// goal or setting it on a freestanding one is the same mistake create_goal
	// rejects.
	mergedTarget := goal.TargetCount
	if c.TargetCount != nil {
		mergedTarget = c.TargetCount
	}
	if err := validateGoalTargetXOR(goal.ItemID, mergedTarget); err != nil {
		return Result{}, err
	}

	// If either period bound moved, the merged range still has to be an ordered,
	// calendar-aligned period of the goal's kind.
	if c.PeriodStart != nil || c.PeriodEnd != nil {
		start := goal.PeriodStart
		if c.PeriodStart != nil {
			start = *c.PeriodStart
		}
		end := goal.PeriodEnd
		if c.PeriodEnd != nil {
			end = *c.PeriodEnd
		}
		if err := validateGoalPeriod(goal.PeriodKind, start, end); err != nil {
			return Result{}, err
		}
	}

	var status *domain.GoalStatus
	if c.Status != nil {
		s := domain.GoalStatus(*c.Status) // Layer 1 has constrained this to "abandoned"
		status = &s
	}

	updated, err := t.store.UpdateGoal(ctx, args.GoalID, store.GoalPatch{
		Title:       c.Title,
		PeriodStart: c.PeriodStart,
		PeriodEnd:   c.PeriodEnd,
		TargetCount: c.TargetCount,
		Status:      status,
	}, time.Now())
	if err != nil {
		return Result{}, wrapValidation(err)
	}

	prog, err := t.store.GoalProgressFor(ctx, updated, t.localZone(ctx))
	if err != nil {
		return Result{}, err
	}
	return Result{Goal: &updated, GoalProgress: &prog}, nil
}

func handleListGoals(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[ListGoalsArgs](raw)
	if err != nil {
		return Result{}, err
	}
	filter := store.GoalFilter(args.Filter)
	if filter == "" {
		filter = store.GoalFilterActive
	}
	goals, err := t.store.ListGoalProgress(ctx, filter, t.localZone(ctx))
	if err != nil {
		return Result{}, err
	}
	return Result{Goals: goals}, nil
}

func handleLogGoalProgress(ctx context.Context, t *Tools, raw json.RawMessage) (Result, error) {
	args, err := decode[LogGoalProgressArgs](raw)
	if err != nil {
		return Result{}, err
	}

	// At least one of progress_pct or note - an empty call writes nothing and
	// is rejected the same shape as any other under-specified tool call.
	if args.ProgressPct == nil && (args.Note == nil || *args.Note == "") {
		return Result{}, domain.Invalid("goal_update_empty", "progress_pct",
			"log_goal_progress needs a progress_pct, a note, or both")
	}

	goal, err := t.store.GetGoal(ctx, args.GoalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, domain.Invalid("goal_exists", "goal_id", "goal %q does not exist", args.GoalID)
		}
		return Result{}, err
	}
	if goal.Status.IsTerminal() {
		return Result{}, domain.Invalid("goal_terminal", "goal_id",
			"goal %q is %s and no longer takes progress updates", args.GoalID, goal.Status)
	}
	// A note about progress on an item-linked goal is harmless, but a percent is
	// misleading: that goal's progress is the chain count, and a logged 60 would
	// never be read back. Point the model at the right tool rather than storing
	// a number nothing surfaces.
	if goal.IsItemLinked() && args.ProgressPct != nil {
		return Result{}, domain.Invalid("goal_update_item_linked", "progress_pct",
			"goal %q tracks progress from its linked item automatically; log_goal_progress with a percent is only for freestanding goals", args.GoalID)
	}

	upd, err := t.store.AppendGoalUpdate(ctx, domain.NewGoalUpdate{
		GoalID:      args.GoalID,
		Note:        args.Note,
		ProgressPct: args.ProgressPct,
		Source:      domain.GoalUpdateByAgent,
	}, time.Now())
	if err != nil {
		return Result{}, wrapValidation(err)
	}

	prog, err := t.store.GoalProgressFor(ctx, goal, t.localZone(ctx))
	if err != nil {
		return Result{}, err
	}
	return Result{GoalUpdate: &upd, Goal: &goal, GoalProgress: &prog}, nil
}

// validateGoalTargetXOR is docs/11-goals-spec.md#validation's two target rows:
// "item-linked goals have a target" (>= 1) and "freestanding goals have no
// target". Deliberately Go and not a SQL CHECK - the clearer message belongs
// beside every other rule.
func validateGoalTargetXOR(itemID *string, target *int) error {
	linked := itemID != nil && *itemID != ""
	switch {
	case linked && (target == nil || *target < 1):
		return domain.Invalid("goal_target_required", "target_count",
			"an item-linked goal needs a target_count of at least 1")
	case !linked && target != nil:
		return domain.Invalid("goal_target_forbidden", "target_count",
			"a freestanding goal (no item_id) takes no target_count")
	default:
		return nil
	}
}

// resolveGoalPeriod turns period_kind plus the optional period_start/period_end
// overrides into a concrete local ISO date range.
//
// day/week/month default to the calendar period containing today (in loc, the
// person's zone). A supplied period_start moves the period but must still name a
// valid boundary - that check is validateGoalPeriod's, run by the caller right
// after this. custom defaults period_start to today and requires period_end.
func resolveGoalPeriod(kind domain.GoalPeriodKind, startArg, endArg *string, now time.Time, loc *time.Location) (string, string, error) {
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC) // civil date, math in UTC

	suppliedStart := startArg != nil && *startArg != ""
	suppliedEnd := endArg != nil && *endArg != ""

	parse := func(field, v string) (time.Time, error) {
		t, err := time.Parse(domain.DateLayout, v)
		if err != nil {
			return time.Time{}, domain.Invalid("goal_period_format", field,
				"%s %q is not a date like 2006-01-02", field, v)
		}
		return t, nil
	}

	if kind == domain.GoalPeriodCustom {
		start := today
		if suppliedStart {
			var err error
			if start, err = parse("period_start", *startArg); err != nil {
				return "", "", err
			}
		}
		if !suppliedEnd {
			return "", "", domain.Invalid("goal_period_end_required", "period_end",
				"period_kind=custom requires period_end")
		}
		end, err := parse("period_end", *endArg)
		if err != nil {
			return "", "", err
		}
		return start.Format(domain.DateLayout), end.Format(domain.DateLayout), nil
	}

	start := today
	if suppliedStart {
		var err error
		if start, err = parse("period_start", *startArg); err != nil {
			return "", "", err
		}
	} else {
		switch kind {
		case domain.GoalPeriodWeek:
			start = today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7)) // back to Monday
		case domain.GoalPeriodMonth:
			start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		}
	}

	end := periodEndFor(kind, start)

	// A supplied period_end on a non-custom goal is only ever a redundant
	// restatement; if it disagrees with the kind's own end it is the "period
	// kind matches range" rule, and validateGoalPeriod will say so.
	if suppliedEnd {
		e, err := parse("period_end", *endArg)
		if err != nil {
			return "", "", err
		}
		end = e
	}

	return start.Format(domain.DateLayout), end.Format(domain.DateLayout), nil
}

// periodEndFor is the inclusive end date of the kind's period starting at start.
func periodEndFor(kind domain.GoalPeriodKind, start time.Time) time.Time {
	switch kind {
	case domain.GoalPeriodWeek:
		return start.AddDate(0, 0, 6)
	case domain.GoalPeriodMonth:
		return start.AddDate(0, 1, 0).AddDate(0, 0, -1)
	default: // day
		return start
	}
}

// validateGoalPeriod is docs/11-goals-spec.md#validation's "period ordered" and
// "period kind matches range" rows. custom is only checked for order; day, week
// and month must be exactly one calendar period, so a redrawn deadline that no
// longer aligns is rejected here rather than stored as a lie.
func validateGoalPeriod(kind domain.GoalPeriodKind, startStr, endStr string) error {
	start, err := time.Parse(domain.DateLayout, startStr)
	if err != nil {
		return domain.Invalid("goal_period_format", "period_start",
			"period_start %q is not a date like 2006-01-02", startStr)
	}
	end, err := time.Parse(domain.DateLayout, endStr)
	if err != nil {
		return domain.Invalid("goal_period_format", "period_end",
			"period_end %q is not a date like 2006-01-02", endStr)
	}
	if end.Before(start) {
		return domain.Invalid("goal_period_order", "period_end",
			"period_end %s is before period_start %s", endStr, startStr)
	}

	switch kind {
	case domain.GoalPeriodDay:
		if !end.Equal(start) {
			return domain.Invalid("goal_period_align", "period_end",
				"a day goal covers one date; period_end %s does not equal period_start %s", endStr, startStr)
		}
	case domain.GoalPeriodWeek:
		if start.Weekday() != time.Monday {
			return domain.Invalid("goal_period_align", "period_start",
				"a week goal starts on a Monday; %s is a %s", startStr, start.Weekday())
		}
		if want := start.AddDate(0, 0, 6); !end.Equal(want) {
			return domain.Invalid("goal_period_align", "period_end",
				"a week goal ends six days after it starts; expected %s, got %s", want.Format(domain.DateLayout), endStr)
		}
	case domain.GoalPeriodMonth:
		if start.Day() != 1 {
			return domain.Invalid("goal_period_align", "period_start",
				"a month goal starts on the first; %s is day %d", startStr, start.Day())
		}
		if want := start.AddDate(0, 1, 0).AddDate(0, 0, -1); !end.Equal(want) {
			return domain.Invalid("goal_period_align", "period_end",
				"a month goal ends on the last day of the month; expected %s, got %s", want.Format(domain.DateLayout), endStr)
		}
	}
	return nil
}
