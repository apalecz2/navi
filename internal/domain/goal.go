package domain

import (
	"fmt"
	"time"
)

// A goal is a target over a period (D-024, docs/11-goals-spec.md). It does not
// fire, is never notified, and completing it once does not resolve it, which is
// why it is its own entity rather than a third Kind. This file is the types and
// their structural rules only; the semantic goal rules — the target/item XOR,
// calendar alignment, the terminal-goal guard — live in internal/agent beside
// the schedule rules, because that is where a rejection can be phrased the way
// the escalation ladder feeds one back to a model.

// GoalPeriodKind is the shape of a goal's window. day/week/month align to the
// calendar; custom carries an explicit range.
type GoalPeriodKind string

const (
	GoalPeriodDay    GoalPeriodKind = "day"
	GoalPeriodWeek   GoalPeriodKind = "week"
	GoalPeriodMonth  GoalPeriodKind = "month"
	GoalPeriodCustom GoalPeriodKind = "custom"
)

func (k GoalPeriodKind) valid() bool {
	switch k {
	case GoalPeriodDay, GoalPeriodWeek, GoalPeriodMonth, GoalPeriodCustom:
		return true
	default:
		return false
	}
}

// GoalStatus is a goal's position in its lifecycle (docs/11-goals-spec.md).
//
//	active ──▶ met        period ends, target reached
//	   │
//	   ├────▶ missed       period ends, target not reached
//	   │
//	   └────▶ abandoned    explicit user instruction, any time
//
// There is no pending and no snoozed: a goal is active from creation, and
// pushing its deadline is an update_goal on period_end, not a resolution.
type GoalStatus string

const (
	GoalActive    GoalStatus = "active"
	GoalMet       GoalStatus = "met"
	GoalMissed    GoalStatus = "missed"
	GoalAbandoned GoalStatus = "abandoned"
)

func (s GoalStatus) valid() bool {
	switch s {
	case GoalActive, GoalMet, GoalMissed, GoalAbandoned:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether the goal's lifecycle has ended. met, missed and
// abandoned are one-way: update_goal and log_goal_progress both reject a goal
// in any of them, and the sweeper's period-end pass writes one exactly once,
// which is what makes an hourly re-run of that pass a no-op.
func (s GoalStatus) IsTerminal() bool {
	switch s {
	case GoalMet, GoalMissed, GoalAbandoned:
		return true
	default:
		return false
	}
}

// GoalUpdateSource records where a goal_updates row came from, matching the
// CHECK on goal_updates.source. briefing joins the set next session.
type GoalUpdateSource string

const (
	GoalUpdateByAgent    GoalUpdateSource = "agent"
	GoalUpdateByWeb      GoalUpdateSource = "web"
	GoalUpdateByBriefing GoalUpdateSource = "briefing"
)

func (s GoalUpdateSource) valid() bool {
	switch s {
	case GoalUpdateByAgent, GoalUpdateByWeb, GoalUpdateByBriefing:
		return true
	default:
		return false
	}
}

// Goal is a stored goal. PeriodStart and PeriodEnd are local ISO dates
// (DateLayout) with no instant of their own — a caller resolves them against a
// zone, the same one the reconciler check-in and pause use, when it needs a
// boundary. ItemID and TargetCount are set together or not at all (the XOR);
// a nil ItemID is a freestanding goal whose progress lives in goal_updates.
type Goal struct {
	ID    string
	Title string

	PeriodKind  GoalPeriodKind
	PeriodStart string
	PeriodEnd   string

	ItemID      *string
	TargetCount *int

	Status GoalStatus

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsItemLinked reports whether progress is a query over the linked item's
// chains rather than a read of the newest goal_updates row.
func (g Goal) IsItemLinked() bool { return g.ItemID != nil }

// GoalUpdate is one append-only progress note on a freestanding goal. The trail
// is the data: current progress is the newest row, never a column that gets
// overwritten, so the log can always answer whether something was trending up
// before it slipped.
type GoalUpdate struct {
	ID          string
	GoalID      string
	Note        *string
	ProgressPct *int
	Source      GoalUpdateSource
	CreatedAt   time.Time
}

// NewGoal is what a caller supplies to create a goal. The store assigns the id,
// the status (always active) and the timestamps. PeriodStart and PeriodEnd are
// already resolved to concrete local ISO dates by the time this is built —
// internal/agent turns period_kind plus the optional overrides into them.
type NewGoal struct {
	Title       string
	PeriodKind  GoalPeriodKind
	PeriodStart string
	PeriodEnd   string
	ItemID      *string
	TargetCount *int
}

// Validate checks what the schema's CHECK constraints would catch plus the
// period ordering, so the error names the field rather than surfacing a driver
// message. The target/item XOR and item existence are internal/agent's, because
// they need a store round trip and a message shaped like the schedule rules.
func (n NewGoal) Validate() error {
	if n.Title == "" {
		return fmt.Errorf("domain: goal title is required")
	}
	if !n.PeriodKind.valid() {
		return fmt.Errorf("domain: goal period_kind %q is not day, week, month or custom", n.PeriodKind)
	}
	start, err := time.Parse(DateLayout, n.PeriodStart)
	if err != nil {
		return fmt.Errorf("domain: goal period_start %q is not a date like 2006-01-02", n.PeriodStart)
	}
	end, err := time.Parse(DateLayout, n.PeriodEnd)
	if err != nil {
		return fmt.Errorf("domain: goal period_end %q is not a date like 2006-01-02", n.PeriodEnd)
	}
	if end.Before(start) {
		return fmt.Errorf("domain: goal period_end %s is before period_start %s", n.PeriodEnd, n.PeriodStart)
	}
	if n.TargetCount != nil && *n.TargetCount < 1 {
		return fmt.Errorf("domain: goal target_count %d is below 1", *n.TargetCount)
	}
	return nil
}

// GoalPeriodBounds turns a goal's local ISO date range into the half-open UTC
// instant range [from, toExclusive) that the chains view's scheduled_at column
// is compared against.
//
// period_end is inclusive per the schema, so toExclusive is midnight at the
// start of the day after it. loc is the person's zone — kv.current_tz then the
// deployment default — the same clock the goal's dates were resolved in and the
// same one the reconciler check-in uses. Day-granular by construction, so
// time.Date is enough here; the DST-exact midnight conversion schedule.Instant
// does is for fire instants, which a goal has none of.
func GoalPeriodBounds(startDate, endDate string, loc *time.Location) (from, toExclusive time.Time, err error) {
	start, err := time.ParseInLocation(DateLayout, startDate, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("domain: goal period_start %q: %w", startDate, err)
	}
	end, err := time.ParseInLocation(DateLayout, endDate, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("domain: goal period_end %q: %w", endDate, err)
	}
	return start.UTC(), end.AddDate(0, 0, 1).UTC(), nil
}

// NewGoalUpdate is what a caller supplies to append a progress row.
type NewGoalUpdate struct {
	GoalID      string
	Note        *string
	ProgressPct *int
	Source      GoalUpdateSource
}

// Validate mirrors the goal_updates CHECK constraints. "At least one of note or
// progress_pct" is internal/agent's (docs/11-goals-spec.md#validation), because
// an under-specified tool call is rejected the same way every other one is.
func (n NewGoalUpdate) Validate() error {
	if n.GoalID == "" {
		return fmt.Errorf("domain: goal_update goal_id is required")
	}
	if !n.Source.valid() {
		return fmt.Errorf("domain: goal_update source %q is not agent, web or briefing", n.Source)
	}
	if n.ProgressPct != nil && (*n.ProgressPct < 0 || *n.ProgressPct > 100) {
		return fmt.Errorf("domain: goal_update progress_pct %d is outside 0..100", *n.ProgressPct)
	}
	return nil
}
