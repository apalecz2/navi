// Package copywriter generates the per-occurrence reminder text ahead of time,
// on the two-pass schedule in G3: a safety-net pass roughly 30 minutes out and a
// regeneration pass roughly 4 minutes out if relevant state changed.
//
// Everything here is best-effort. Failure is bounded by an attempt counter and
// leaves message_text null, which the scheduler reads as "send the plain title"
// (G4). A stalled copywriter is a cosmetic outage, not a delivery one.
//
// The tick body is a no-op in this session.
package copywriter

import (
	"context"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "copywriter"

// Interval is the poll interval. The tighter of the two passes is four minutes
// out, so a minute of granularity is enough.
const Interval = 60 * time.Second

// Copywriter generates occurrence message text.
type Copywriter struct {
	log *slog.Logger
}

// New returns a copywriter.
func New(log *slog.Logger) *Copywriter {
	return &Copywriter{log: log}
}

// Loop describes this loop to the supervisor.
func (c *Copywriter) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: c.Tick}
}

// Tick does nothing yet. The real body selects occurrences inside either
// generation window and fills message_text.
func (c *Copywriter) Tick(ctx context.Context) error {
	c.log.Debug("tick")
	return nil
}
