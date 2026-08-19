// Package sweeper is the backstop for the other loops: snooze-cap enforcement,
// log retention, and re-running materialization when the horizon has thinned
// because a nightly run was missed.
//
// The horizon backfill and llm_calls retention (L7, 90 days per
// 04-data-model.md) exist so far. conversations' own retention (180 days)
// is not built yet.
//
// Snooze-cap enforcement is listed against this loop in 03-architecture.md's
// loop table and deliberately has no code here, because there is nothing for an
// hourly pass to find. The cap is checked before a child is written
// (domain.CheckSnoozeCap, in store.SnoozeOccurrence), so no row can ever exist
// above it, and a chain sitting exactly at the cap has an ordinary pending live
// link the scheduler fires like any other. Past the cap the snooze request
// itself resolves the chain as missed (R8) — synchronously, at the moment it is
// asked for, which is also what keeps that missed honest under D-008.
//
// The one state that looks like work is lowering items.snooze_cap after
// children already exist. It is not: the live child still fires, and the next
// snooze request is refused at the endpoint against the new cap. Do not add a
// pass here on the strength of the loop table's wording alone.
package sweeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "sweeper"

// Interval is the poll interval. Nothing this loop does is urgent; everything
// it does is a correction to something else that was.
const Interval = time.Hour

// MinHorizonDays is the horizon below which materialization is re-run. The
// materializer targets 30 days, so five days of slack absorbs a missed night
// without reacting to one.
const MinHorizonDays = 25

// LLMCallRetention is L7's retention policy for llm_calls, per
// 04-data-model.md: 90 days, because it is the largest table by row count
// within a month and nothing older is useful once the ladder is tuned.
const LLMCallRetention = 90 * 24 * time.Hour

// Store is the narrow view this loop needs. Declared here rather than taken as
// the concrete type because the horizon and llm_calls retention are the only
// things it asks the database for, and saying so is what keeps the backstop
// from growing into a second scheduler.
type Store interface {
	Horizon(ctx context.Context) (int, bool, error)
	PruneLLMCalls(ctx context.Context, before time.Time) (int64, error)
}

// Materializer is the one call this loop makes into the expansion path. The
// sweeper decides when the horizon is too short; it does not know how a schedule
// becomes rows, and it should not learn.
type Materializer interface {
	All(ctx context.Context) (materializer.Result, error)
}

// Sweeper enforces caps, applies retention, and backfills materialization.
type Sweeper struct {
	log   *slog.Logger
	store Store
	mat   Materializer
}

// New returns a sweeper.
func New(log *slog.Logger, st Store, mat Materializer) *Sweeper {
	return &Sweeper{log: log, store: st, mat: mat}
}

// Loop describes this loop to the supervisor.
func (s *Sweeper) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: s.Tick}
}

// Tick re-runs materialization when the horizon has thinned, then prunes
// llm_calls rows past their retention window.
//
// An absent horizon counts as thin. A database that has never materialized is
// either brand new or has had every nightly run since it was created fail, and
// both want the same thing done about them within the hour.
func (s *Sweeper) Tick(ctx context.Context) error {
	days, ok, err := s.store.Horizon(ctx)
	if err != nil {
		return err
	}
	if !ok || days < MinHorizonDays {
		res, err := s.mat.All(ctx)
		if err != nil {
			return err
		}
		s.log.Info("horizon backfilled", "was_days", days, "had_horizon", ok, "result", res)
	}

	pruned, err := s.store.PruneLLMCalls(ctx, time.Now().Add(-LLMCallRetention))
	if err != nil {
		return err
	}
	if pruned > 0 {
		s.log.Info("llm_calls pruned", "rows", pruned)
	}
	return nil
}
