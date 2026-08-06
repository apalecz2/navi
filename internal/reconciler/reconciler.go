// Package reconciler runs the daily check-in and, after a grace window, assigns
// missed.
//
// The ordering is the point: missed means "asked and got nothing", not
// "midnight passed" (D-008, K6). Nothing in this package may mark an occurrence
// missed on a clock alone, which is also why a transport outage produces no
// false misses — an unsent check-in never asked.
//
// The tick body is a no-op in this session.
package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "reconciler"

// Interval is how often the loop wakes. It acts at configured local times, so
// the body is a gate that returns immediately on most ticks.
const Interval = 60 * time.Second

// Reconciler sends the daily check-in and applies missed after grace.
type Reconciler struct {
	log *slog.Logger
}

// New returns a reconciler.
func New(log *slog.Logger) *Reconciler {
	return &Reconciler{log: log}
}

// Loop describes this loop to the supervisor.
func (r *Reconciler) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: r.Tick}
}

// Tick does nothing yet. The real body checks the configured local
// reconciliation time, sends one consolidated message covering everything
// unresolved (K4), and applies missed to whatever the grace window closes on.
func (r *Reconciler) Tick(ctx context.Context) error {
	r.log.Debug("tick")
	return nil
}
