// Package materializer expands item schedules into occurrence rows ahead of
// time (D-003). Randomness resolves here rather than at fire time, which is what
// makes a windowed or fuzzy reminder an ordinary scheduled one everywhere
// downstream (D-005).
//
// The tick body is a no-op in this session.
package materializer

import (
	"context"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "materializer"

// Interval is how often the loop wakes, not how often it does work. The loop
// table calls this one nightly, and the nightly gate belongs in the tick body:
// a bare 24h ticker would report a stale last_tick within minutes of every
// restart and would contribute one sample a day to the tick-interval series,
// which is not enough to see the loop slowing down before it stops.
const Interval = 60 * time.Second

// HorizonDays is how far ahead occurrences are materialized (I3). The hourly
// sweeper re-runs materialization when the real horizon falls under 25 days.
const HorizonDays = 30

// Materializer expands schedules into occurrences.
type Materializer struct {
	log *slog.Logger
}

// New returns a materializer.
func New(log *slog.Logger) *Materializer {
	return &Materializer{log: log}
}

// Loop describes this loop to the supervisor.
func (m *Materializer) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: m.Tick}
}

// Tick does nothing yet. The real body checks whether a run has happened since
// the last nightly boundary, and expands schedules to the horizon if not.
func (m *Materializer) Tick(ctx context.Context) error {
	m.log.Debug("tick", "horizon_days", HorizonDays)
	return nil
}
