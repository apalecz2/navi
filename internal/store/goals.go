package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// Goals (P3.5, docs/11-goals-spec.md). Two shapes in one table: item-linked,
// whose progress is a live count over the chains view, and freestanding, whose
// progress is the newest goal_updates row. Neither has a stored progress
// counter — a number that can silently disagree with the rows it summarises is
// exactly the drift D-024's "chains, not rows" reasoning already rejected once.

func toDomainGoal(row sqlc.Goal) (domain.Goal, error) {
	createdAt, err := domain.ParseTime(row.CreatedAt)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("goal %s created_at: %w", row.ID, err)
	}
	updatedAt, err := domain.ParseTime(row.UpdatedAt)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("goal %s updated_at: %w", row.ID, err)
	}
	return domain.Goal{
		ID:          row.ID,
		Title:       row.Title,
		PeriodKind:  domain.GoalPeriodKind(row.PeriodKind),
		PeriodStart: row.PeriodStart,
		PeriodEnd:   row.PeriodEnd,
		ItemID:      row.ItemID,
		TargetCount: intPtr(row.TargetCount),
		Status:      domain.GoalStatus(row.Status),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}, nil
}

func toDomainGoalUpdate(row sqlc.GoalUpdate) (domain.GoalUpdate, error) {
	createdAt, err := domain.ParseTime(row.CreatedAt)
	if err != nil {
		return domain.GoalUpdate{}, fmt.Errorf("goal_update %s created_at: %w", row.ID, err)
	}
	return domain.GoalUpdate{
		ID:          row.ID,
		GoalID:      row.GoalID,
		Note:        row.Note,
		ProgressPct: intPtr(row.ProgressPct),
		Source:      domain.GoalUpdateSource(row.Source),
		CreatedAt:   createdAt,
	}, nil
}

// GoalFilter is list_goals' filter argument (docs/06-agent-spec.md).
type GoalFilter string

const (
	GoalFilterActive GoalFilter = "active"
	GoalFilterAll    GoalFilter = "all"
)

// CreateGoal writes a goal. The id, the status (always active) and the
// timestamps are assigned here. The caller has already run the semantic rules
// (the target/item XOR, item existence, calendar alignment) in internal/agent;
// this runs the structural ones and the write.
func (s *Store) CreateGoal(ctx context.Context, n domain.NewGoal, now time.Time) (domain.Goal, error) {
	if err := n.Validate(); err != nil {
		return domain.Goal{}, err
	}
	nowText := domain.FormatTime(now)
	params := sqlc.CreateGoalParams{
		ID:          domain.NewID(),
		Title:       n.Title,
		PeriodKind:  string(n.PeriodKind),
		PeriodStart: n.PeriodStart,
		PeriodEnd:   n.PeriodEnd,
		ItemID:      n.ItemID,
		TargetCount: int64Ptr(n.TargetCount),
		Status:      string(domain.GoalActive),
		CreatedAt:   nowText,
		UpdatedAt:   nowText,
	}

	var row sqlc.Goal
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		row, err = q.CreateGoal(ctx, params)
		return err
	})
	if err != nil {
		return domain.Goal{}, fmt.Errorf("store: create goal: %w", err)
	}
	return toDomainGoal(row)
}

// GetGoal returns one goal, or ErrNotFound.
func (s *Store) GetGoal(ctx context.Context, id string) (domain.Goal, error) {
	row, err := s.read.GetGoal(ctx, id)
	if err != nil {
		return domain.Goal{}, notFound("store: get goal", err)
	}
	return toDomainGoal(row)
}

// ListGoals returns goals filtered to active or all, ordered deterministically.
// An unrecognised filter falls back to active; Layer 1 in internal/agent has
// already rejected a bad one by the time this runs.
func (s *Store) ListGoals(ctx context.Context, filter GoalFilter) ([]domain.Goal, error) {
	var rows []sqlc.Goal
	var err error
	if filter == GoalFilterAll {
		rows, err = s.read.ListAllGoals(ctx)
	} else {
		rows, err = s.read.ListActiveGoals(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list goals: %w", err)
	}
	goals := make([]domain.Goal, 0, len(rows))
	for _, row := range rows {
		g, err := toDomainGoal(row)
		if err != nil {
			return nil, fmt.Errorf("store: list goals: %w", err)
		}
		goals = append(goals, g)
	}
	return goals, nil
}

// GoalPatch is a partial update_goal edit: a nil field leaves its column alone.
// PeriodKind is not here — changing the shape of the window is a new goal, not
// an edit — and neither is a way to clear TargetCount back to NULL, matching
// ItemPatch's own convention.
type GoalPatch struct {
	Title       *string
	PeriodStart *string
	PeriodEnd   *string
	TargetCount *int
	Status      *domain.GoalStatus
}

// UpdateGoal applies patch to an existing goal. The terminal-goal guard is
// internal/agent's (Layer 2), so this is only reached for an active goal.
func (s *Store) UpdateGoal(ctx context.Context, id string, patch GoalPatch, now time.Time) (domain.Goal, error) {
	var out domain.Goal
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetGoal(ctx, id)
		if err != nil {
			return notFound("store: update goal", err)
		}
		cur, err := toDomainGoal(current)
		if err != nil {
			return err
		}

		merged := cur
		if patch.Title != nil {
			merged.Title = *patch.Title
		}
		if patch.PeriodStart != nil {
			merged.PeriodStart = *patch.PeriodStart
		}
		if patch.PeriodEnd != nil {
			merged.PeriodEnd = *patch.PeriodEnd
		}
		if patch.TargetCount != nil {
			merged.TargetCount = patch.TargetCount
		}
		if patch.Status != nil {
			merged.Status = *patch.Status
		}

		row, err := q.UpdateGoal(ctx, sqlc.UpdateGoalParams{
			Title:       merged.Title,
			PeriodStart: merged.PeriodStart,
			PeriodEnd:   merged.PeriodEnd,
			TargetCount: int64Ptr(merged.TargetCount),
			Status:      string(merged.Status),
			UpdatedAt:   domain.FormatTime(now),
			ID:          id,
		})
		if err != nil {
			return notFound("store: update goal", err)
		}
		out, err = toDomainGoal(row)
		return err
	})
	if err != nil {
		return domain.Goal{}, fmt.Errorf("store: update goal: %w", err)
	}
	return out, nil
}

// AppendGoalUpdate writes one progress row. Append-only: there is no update path
// and no delete path, because the trail is the data (docs/11-goals-spec.md).
func (s *Store) AppendGoalUpdate(ctx context.Context, n domain.NewGoalUpdate, now time.Time) (domain.GoalUpdate, error) {
	if err := n.Validate(); err != nil {
		return domain.GoalUpdate{}, err
	}
	params := sqlc.CreateGoalUpdateParams{
		ID:          domain.NewID(),
		GoalID:      n.GoalID,
		Note:        n.Note,
		ProgressPct: int64Ptr(n.ProgressPct),
		Source:      string(n.Source),
		CreatedAt:   domain.FormatTime(now),
	}

	var row sqlc.GoalUpdate
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		row, err = q.CreateGoalUpdate(ctx, params)
		return err
	})
	if err != nil {
		return domain.GoalUpdate{}, fmt.Errorf("store: append goal update: %w", err)
	}
	return toDomainGoalUpdate(row)
}

// LatestGoalUpdate returns a freestanding goal's newest progress row, or
// ErrNotFound when it has none — which the period-end pass reads as missed.
func (s *Store) LatestGoalUpdate(ctx context.Context, goalID string) (domain.GoalUpdate, error) {
	row, err := s.read.LatestGoalUpdate(ctx, goalID)
	if err != nil {
		return domain.GoalUpdate{}, notFound("store: latest goal update", err)
	}
	return toDomainGoalUpdate(row)
}

// countCompletedChainsQuery counts snooze chains for one item whose root falls
// in a half-open instant range and that completed (any link). It reads the
// chains view directly, which sqlc cannot see (sqlc.yaml), so it is
// hand-written database/sql like chains.go — the same and only exception.
const countCompletedChainsQuery = `
SELECT count(*) FROM chains
WHERE item_id = ?
  AND scheduled_at >= ?
  AND scheduled_at < ?
  AND was_completed = 1`

// CountCompletedChains is the one code path for "how many of the target
// happened this period" (V6). Item-linked goal progress, goal velocity, and —
// when P4 builds them — get_stats and the stats endpoint all call this. There
// is no counter and no goals-table write on completion: the number is
// recomputed from chains on every read, which is why resolving an occurrence
// moves it with nothing touching the goal.
func (s *Store) CountCompletedChains(ctx context.Context, itemID string, from, toExclusive time.Time) (int, error) {
	var n int64
	err := s.reader.QueryRowContext(ctx, countCompletedChainsQuery,
		itemID, domain.FormatTime(from), domain.FormatTime(toExclusive),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count completed chains for item %s: %w", itemID, err)
	}
	return int(n), nil
}

// GoalProgress is a goal plus its current progress, computed fresh. It is what
// context injection, list_goals and the period-end pass all read, so the number
// the agent states and the number the briefing (next session) states cannot
// disagree.
type GoalProgress struct {
	Goal       domain.Goal
	ItemLinked bool

	// Item-linked: completed chains in the period, against the target.
	Completed int
	Target    int

	// Freestanding: the newest goal_updates row, if any.
	LatestPct  *int
	LatestNote *string
	UpdatedAt  *time.Time
	HasUpdate  bool
}

// Met reports whether the goal's target is currently reached — the same test
// the period-end pass applies, exposed so a caller never re-derives it.
func (p GoalProgress) Met() bool {
	if p.ItemLinked {
		return p.Completed >= p.Target
	}
	return p.LatestPct != nil && *p.LatestPct >= 100
}

// Velocity is completions per elapsed week for an item-linked goal, read from
// the same chains count as progress, never a stored counter. nil for a
// freestanding goal, which has no completion event to rate.
//
// Per week rather than per period-kind so a "gym 10 times over three weeks"
// custom goal has a meaningful rate; for a one-week goal exactly one week has
// elapsed, so this equals the raw completed count, which is what a hand count
// over chains checks.
func (p GoalProgress) Velocity(now time.Time, loc *time.Location) *float64 {
	if !p.ItemLinked {
		return nil
	}
	start, err := time.ParseInLocation(domain.DateLayout, p.Goal.PeriodStart, loc)
	if err != nil {
		return nil
	}
	weeks := math.Ceil(now.Sub(start).Hours() / (24 * 7))
	if weeks < 1 {
		weeks = 1
	}
	v := float64(p.Completed) / weeks
	return &v
}

// GoalProgressFor computes one goal's progress against loc (the person's zone).
func (s *Store) GoalProgressFor(ctx context.Context, g domain.Goal, loc *time.Location) (GoalProgress, error) {
	p := GoalProgress{Goal: g, ItemLinked: g.IsItemLinked()}

	if g.IsItemLinked() {
		from, toExcl, err := domain.GoalPeriodBounds(g.PeriodStart, g.PeriodEnd, loc)
		if err != nil {
			return GoalProgress{}, err
		}
		p.Completed, err = s.CountCompletedChains(ctx, *g.ItemID, from, toExcl)
		if err != nil {
			return GoalProgress{}, err
		}
		if g.TargetCount != nil {
			p.Target = *g.TargetCount
		}
		return p, nil
	}

	upd, err := s.LatestGoalUpdate(ctx, g.ID)
	if errors.Is(err, ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return GoalProgress{}, err
	}
	p.HasUpdate = true
	p.LatestPct = upd.ProgressPct
	p.LatestNote = upd.Note
	u := upd.CreatedAt
	p.UpdatedAt = &u
	return p, nil
}

// ListGoalProgress computes progress for every goal matching filter, in one
// call, for context injection.
func (s *Store) ListGoalProgress(ctx context.Context, filter GoalFilter, loc *time.Location) ([]GoalProgress, error) {
	goals, err := s.ListGoals(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]GoalProgress, 0, len(goals))
	for _, g := range goals {
		p, err := s.GoalProgressFor(ctx, g, loc)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GoalEvaluation is what one period-end pass did, for the sweeper's log line
// and for naviseed.
type GoalEvaluation struct {
	Considered int
	Met        int
	Missed     int
}

// EvaluateDueGoals is the sweeper's period-end pass (docs/11-goals-spec.md#
// lifecycle). Code, not the clock: a goal leaves active only when this reads
// period_end < today and computes the outcome. Idempotent by construction —
// the SELECT filters status = 'active', the write is one-way, so a redundant
// hourly run touches nothing.
//
// now and loc are the person's clock, resolved by the caller the same way the
// reconciler's grace pass resolves its zone, so "the period ended" agrees with
// how the goal's dates were written.
func (s *Store) EvaluateDueGoals(ctx context.Context, now time.Time, loc *time.Location) (GoalEvaluation, error) {
	today := now.In(loc).Format(domain.DateLayout)

	rows, err := s.read.ListGoalsPastPeriod(ctx, today)
	if err != nil {
		return GoalEvaluation{}, fmt.Errorf("store: evaluate due goals: %w", err)
	}

	res := GoalEvaluation{Considered: len(rows)}
	nowText := domain.FormatTime(now)

	for _, row := range rows {
		g, err := toDomainGoal(row)
		if err != nil {
			return res, fmt.Errorf("store: evaluate due goals: %w", err)
		}

		prog, err := s.GoalProgressFor(ctx, g, loc)
		if err != nil {
			return res, fmt.Errorf("store: evaluate due goals: goal %s: %w", g.ID, err)
		}
		outcome := domain.GoalMissed
		if prog.Met() {
			outcome = domain.GoalMet
		}

		affected, err := s.evalWrite(ctx, g.ID, outcome, nowText)
		if err != nil {
			return res, fmt.Errorf("store: evaluate due goals: goal %s: %w", g.ID, err)
		}
		if affected == 0 {
			continue // a concurrent update_goal already moved it off active
		}
		if outcome == domain.GoalMet {
			res.Met++
		} else {
			res.Missed++
		}
	}
	return res, nil
}

// evalWrite is one guarded status write, its own tiny transaction. Per goal
// rather than one transaction for the batch: the pass is a backstop with no
// atomicity requirement across goals, and a long write transaction would hold
// the single writer connection the way a nightly materialize run must not.
func (s *Store) evalWrite(ctx context.Context, id string, status domain.GoalStatus, nowText string) (int64, error) {
	var affected int64
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		var err error
		affected, err = q.SetGoalStatus(ctx, sqlc.SetGoalStatusParams{
			Status:    string(status),
			UpdatedAt: nowText,
			ID:        id,
		})
		return err
	})
	return affected, err
}
