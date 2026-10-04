package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Goal kinds, as GoalSeries.Kind reports them.
const (
	GoalKindItemLinked  = "item_linked"
	GoalKindFreestanding = "freestanding"
)

// GoalPoint is one point on a goal's progress line. Both goal kinds produce the
// same point so a chart never branches on which it is drawing.
//
// At is the instant the value holds from (UTC, domain.FormatTime) and Date its
// local date. Value is a completion count for an item-linked goal and a percent
// for a freestanding one, null on an update that carried only a note. Velocity is
// item-linked only; a freestanding goal has no completion event to rate
// (docs/11-goals-spec.md, Velocity), so it is null there rather than a slope.
type GoalPoint struct {
	Date            string   `json:"date"`
	At              string   `json:"at"`
	Value           *int     `json:"value"`
	VelocityPerWeek *float64 `json:"velocity_per_week"`
	Note            *string  `json:"note"`
}

// GoalSeries is GET /api/goals/{id}/progress.
type GoalSeries struct {
	GoalID      string  `json:"goal_id"`
	Title       string  `json:"title"`
	Status      string  `json:"status"`
	Kind        string  `json:"kind"`
	Unit        string  `json:"unit"` // "completions" or "percent"
	PeriodKind  string  `json:"period_kind"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
	Timezone    string  `json:"timezone"`
	ItemID      *string `json:"item_id"`

	// Target is the count to reach, or 100 for a freestanding goal. Current is the
	// same figure the summary's goal row reports, taken from the same
	// store.GoalProgress, so the headline and the last point cannot differ.
	Target          int      `json:"target"`
	Current         *int     `json:"current"`
	VelocityPerWeek *float64 `json:"velocity_per_week"`
	Met             bool     `json:"met"`

	Points []GoalPoint `json:"points"`
}

// GoalProgress is the progress series for one goal.
//
// An item-linked goal's series is store.CountCompletedChains evaluated at the end
// of each local day of the period, up to today: "chains scheduled in the period
// up to that day that completed", the same count the goal's progress is, so the
// last point is the goal's own figure and not a second tally. A freestanding
// goal's is its goal_updates trail as written. Neither kind has a query here that
// the summary does not already use.
func (s *Service) GoalProgress(ctx context.Context, id string) (GoalSeries, error) {
	if !domain.ValidID(id) {
		return GoalSeries{}, domain.Invalid("stats_goal_id", "id", "goal id %q is not a goal id", id)
	}
	g, err := s.r.GetGoal(ctx, id)
	if err != nil {
		return GoalSeries{}, err // store.ErrNotFound reaches the handler as itself
	}
	zones, err := schedule.LoadZones(ctx, s.r, s.defaultTZ)
	if err != nil {
		return GoalSeries{}, fmt.Errorf("stats: resolve zones: %w", err)
	}
	loc := zones.Local()
	now := s.now()

	p, err := s.r.GoalProgressFor(ctx, g, loc)
	if err != nil {
		return GoalSeries{}, fmt.Errorf("stats: goal %s: %w", id, err)
	}

	out := GoalSeries{
		GoalID: g.ID, Title: g.Title, Status: string(g.Status),
		PeriodKind: string(g.PeriodKind), PeriodStart: g.PeriodStart, PeriodEnd: g.PeriodEnd,
		Timezone: loc.String(), ItemID: g.ItemID, Met: p.Met(), Points: []GoalPoint{},
	}

	if !p.ItemLinked {
		out.Kind, out.Unit, out.Target, out.Current = GoalKindFreestanding, "percent", 100, p.LatestPct
		ups, err := s.r.ListGoalUpdates(ctx, g.ID)
		if err != nil {
			return GoalSeries{}, err
		}
		for _, u := range ups {
			out.Points = append(out.Points, GoalPoint{
				Date:  u.CreatedAt.In(loc).Format(domain.DateLayout),
				At:    domain.FormatTime(u.CreatedAt),
				Value: u.ProgressPct, Note: u.Note,
			})
		}
		return out, nil
	}

	out.Kind, out.Unit, out.Target = GoalKindItemLinked, "completions", p.Target
	cur := p.Completed
	out.Current = &cur
	if v := p.Velocity(now, loc); v != nil {
		out.VelocityPerWeek = round(*v, 2)
	}

	from, toExcl, err := domain.GoalPeriodBounds(g.PeriodStart, g.PeriodEnd, loc)
	if err != nil {
		return GoalSeries{}, err
	}
	today := dayStart(now, loc)
	last := dayStart(toExcl.Add(-time.Second), loc) // the period's last local day
	final := last
	if today.Before(final) {
		final = today
	}
	for d := dayStart(from, loc); !d.After(final); d = time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc) {
		end := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc)
		var n int
		at := end.Add(-time.Second) // the day's last second, so the axis labels it with its own date
		if d.Equal(final) {
			// The closing point is the goal's own progress, whole-period bound and
			// all, so the line ends exactly on the headline figure. Mid-period that
			// is "as of now"; the instant says so.
			n = p.Completed
			if d.Equal(today) {
				at = now.Truncate(time.Second)
			}
		} else if n, err = s.r.CountCompletedChains(ctx, *g.ItemID, from, end); err != nil {
			return GoalSeries{}, err
		}
		pt := GoalPoint{Date: d.Format(domain.DateLayout), At: domain.FormatTime(at), Value: &n}
		at1 := store.GoalProgress{Goal: g, ItemLinked: true, Completed: n}
		pt.VelocityPerWeek = at1.Velocity(at, loc)
		if pt.VelocityPerWeek != nil {
			pt.VelocityPerWeek = round(*pt.VelocityPerWeek, 2)
		}
		out.Points = append(out.Points, pt)
	}
	return out, nil
}
