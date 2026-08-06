// Package sweeper is the backstop for the other loops: snooze-cap enforcement,
// log retention, and re-running materialization when the horizon has thinned
// because a nightly run was missed.
//
// The tick body is a no-op in this session.
package sweeper

import (
	"context"
	"log/slog"
	"time"

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

// Sweeper enforces caps, applies retention, and backfills materialization.
type Sweeper struct {
	log *slog.Logger
}

// New returns a sweeper.
func New(log *slog.Logger) *Sweeper {
	return &Sweeper{log: log}
}

// Loop describes this loop to the supervisor.
func (s *Sweeper) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: s.Tick}
}

// Tick does nothing yet.
func (s *Sweeper) Tick(ctx context.Context) error {
	s.log.Debug("tick", "min_horizon_days", MinHorizonDays)
	return nil
}
