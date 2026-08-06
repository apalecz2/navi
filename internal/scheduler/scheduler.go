// Package scheduler is the fire path. It polls occurrence rows, claims the ones
// that are due with BEGIN IMMEDIATE, and sends them.
//
// There is no model call in this package and there never will be one. A reminder
// fires because a SQLite row says it is due; every model contribution is
// generated in advance, stored on the row, and degrades to the plain item title
// when absent (D-001, N3). The import block of this package is where that
// invariant is enforced.
//
// The tick body is a no-op in this session.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/aidenpaleczny/navi/internal/supervisor"
)

// Name is the loop's identity in logs, /healthz, and the metric labels.
const Name = "scheduler"

// Interval is the poll interval. Q1 allows a minute between the scheduled time
// and delivery; polling at 30s spends half of that budget and leaves the rest
// for the transport.
const Interval = 30 * time.Second

// Scheduler claims due occurrences and sends notifications.
type Scheduler struct {
	log *slog.Logger
}

// New returns a scheduler.
func New(log *slog.Logger) *Scheduler {
	return &Scheduler{log: log}
}

// Loop describes this loop to the supervisor.
func (s *Scheduler) Loop() supervisor.Loop {
	return supervisor.Loop{Name: Name, Interval: Interval, Tick: s.Tick}
}

// Tick does nothing yet. The real body claims due rows in one BEGIN IMMEDIATE
// transaction, then sends occurrence.message_text or falls back to item.title.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.log.Debug("tick")
	return nil
}
